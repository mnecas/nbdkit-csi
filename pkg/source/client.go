package source

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

const (
	kubeletPluginsDir = "/var/lib/kubelet/plugins"
	StagingBaseDir    = "/var/lib/kubelet/plugins/nbdkit.csi.k8s.io/source-staging"
)

type SourceStager struct {
	k8sClient kubernetes.Interface
}

func NewSourceStager() (*SourceStager, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create k8s client: %w", err)
	}

	if err := os.MkdirAll(StagingBaseDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create staging base dir: %w", err)
	}

	return &SourceStager{
		k8sClient: clientset,
	}, nil
}

// StageSourceVolume looks up the source PV, connects to its CSI driver, and
// stages the volume on this node. Returns the staging path and the driver name.
func (s *SourceStager) StageSourceVolume(ctx context.Context, pvName, nodeID string) (string, string, error) {
	pv, err := s.k8sClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to get PV %q: %w", pvName, err)
	}

	if pv.Spec.CSI == nil {
		return "", "", fmt.Errorf("PV %q is not a CSI volume", pvName)
	}

	csiSource := pv.Spec.CSI
	driverName := csiSource.Driver
	volumeHandle := csiSource.VolumeHandle
	volumeContext := csiSource.VolumeAttributes

	klog.Infof("Source PV %q: driver=%s, handle=%s", pvName, driverName, volumeHandle)

	stagingPath := filepath.Join(StagingBaseDir, pvName)
	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		return "", "", fmt.Errorf("failed to create staging path: %w", err)
	}

	// Read secrets if referenced
	secrets, err := s.resolveSecrets(ctx, csiSource)
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve secrets: %w", err)
	}

	// Connect to the source CSI driver's socket
	socketPath, err := findDriverSocket(driverName)
	if err != nil {
		return "", "", fmt.Errorf("failed to find socket for driver %s: %w", driverName, err)
	}
	conn, err := grpcConnect(socketPath)
	if err != nil {
		return "", "", fmt.Errorf("failed to connect to source CSI driver at %s: %w", socketPath, err)
	}
	defer conn.Close()

	nodeClient := csi.NewNodeClient(conn)

	// Try direct mount first (more reliable than CSI driver calls due to mount namespace issues)
	if mounted, err := directMount(driverName, volumeHandle, volumeContext, stagingPath); mounted {
		klog.Infof("Source volume %s directly mounted at %s", pvName, stagingPath)
		return stagingPath, driverName, nil
	} else if err != nil {
		klog.Warningf("Direct mount failed for %s, falling back to CSI: %v", driverName, err)
	}

	// Fallback: call the source CSI driver
	supportsStage := driverSupportsStaging(ctx, nodeClient)
	volCap := buildVolumeCapability(pv)

	if supportsStage {
		stageReq := &csi.NodeStageVolumeRequest{
			VolumeId:          volumeHandle,
			StagingTargetPath: stagingPath,
			VolumeCapability:  volCap,
			VolumeContext:     volumeContext,
			Secrets:           secrets,
		}
		klog.Infof("Calling NodeStageVolume on %s for volume %s", driverName, volumeHandle)
		_, err = nodeClient.NodeStageVolume(ctx, stageReq)
		if err != nil {
			return "", "", fmt.Errorf("source CSI NodeStageVolume failed: %w", err)
		}
	} else {
		publishReq := &csi.NodePublishVolumeRequest{
			VolumeId:         volumeHandle,
			TargetPath:       stagingPath,
			VolumeCapability: volCap,
			VolumeContext:    volumeContext,
			Secrets:          secrets,
		}
		klog.Infof("Calling NodePublishVolume on %s for volume %s", driverName, volumeHandle)
		_, err = nodeClient.NodePublishVolume(ctx, publishReq)
		if err != nil {
			return "", "", fmt.Errorf("source CSI NodePublishVolume failed: %w", err)
		}
	}

	klog.Infof("Source volume %s mounted at %s (driver: %s)", pvName, stagingPath, driverName)
	return stagingPath, driverName, nil
}

func driverSupportsStaging(ctx context.Context, nodeClient csi.NodeClient) bool {
	resp, err := nodeClient.NodeGetCapabilities(ctx, &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		return false
	}
	for _, cap := range resp.GetCapabilities() {
		if rpc := cap.GetRpc(); rpc != nil {
			if rpc.GetType() == csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME {
				return true
			}
		}
	}
	return false
}

// UnstageSourceVolume unmounts the source volume.
func (s *SourceStager) UnstageSourceVolume(ctx context.Context, pvName string) error {
	stagingPath := filepath.Join(StagingBaseDir, pvName)

	// Just unmount directly -- works for both direct mounts and CSI-triggered mounts
	if err := DirectUnmount(stagingPath); err != nil {
		klog.Warningf("Direct unmount of %s failed: %v", stagingPath, err)
	}

	_ = os.RemoveAll(stagingPath)
	klog.Infof("Source volume %s unmounted", pvName)
	return nil
}

// directMount mounts the source volume directly without going through the CSI driver.
// This avoids mount namespace issues when calling another driver's NodePublishVolume.
func directMount(driverName, volumeHandle string, volumeContext map[string]string, targetPath string) (bool, error) {
	switch {
	case strings.Contains(driverName, "nfs"):
		return mountNFS(volumeHandle, targetPath)
	default:
		return false, nil
	}
}

// mountNFS parses an NFS CSI volume handle (server#share#subpath#) and mounts it.
func mountNFS(volumeHandle, targetPath string) (bool, error) {
	parts := strings.Split(volumeHandle, "#")
	if len(parts) < 3 {
		return false, fmt.Errorf("cannot parse NFS volume handle: %s", volumeHandle)
	}

	server := parts[0]
	share := "/" + parts[1]
	subPath := parts[2]

	nfsSource := fmt.Sprintf("%s:%s", server, share)
	if subPath != "" {
		nfsSource = fmt.Sprintf("%s/%s", nfsSource, subPath)
	}

	klog.Infof("Mounting NFS %s at %s", nfsSource, targetPath)
	cmd := exec.Command("mount", "-t", "nfs", nfsSource, targetPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("mount -t nfs failed: %w, output: %s", err, string(output))
	}
	return true, nil
}

// DirectUnmount unmounts a directly-mounted source volume.
func DirectUnmount(targetPath string) error {
	cmd := exec.Command("umount", targetPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("umount failed: %w, output: %s", err, string(output))
	}
	return nil
}

func buildVolumeCapability(pv *corev1.PersistentVolume) *csi.VolumeCapability {
	// Check PV's volume mode
	if pv.Spec.VolumeMode != nil && *pv.Spec.VolumeMode == corev1.PersistentVolumeBlock {
		return &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{
				Block: &csi.VolumeCapability_BlockVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		}
	}
	// Default to filesystem mount
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func (s *SourceStager) resolveSecrets(ctx context.Context, csiSource *corev1.CSIPersistentVolumeSource) (map[string]string, error) {
	ref := csiSource.NodeStageSecretRef
	if ref == nil {
		return nil, nil
	}

	secret, err := s.k8sClient.CoreV1().Secrets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to get secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	result := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		result[k] = string(v)
	}
	return result, nil
}

// findDriverSocket locates the CSI socket for a given driver name.
// The directory name doesn't always match the driver name, so we scan
// all plugin directories and call GetPluginInfo to identify the right one.
func findDriverSocket(driverName string) (string, error) {
	// Try exact match first
	exactPath := filepath.Join(kubeletPluginsDir, driverName, "csi.sock")
	if _, err := os.Stat(exactPath); err == nil {
		return exactPath, nil
	}

	// Scan all plugin directories for a csi.sock and check which driver they serve
	entries, err := os.ReadDir(kubeletPluginsDir)
	if err != nil {
		return "", fmt.Errorf("cannot read plugins dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sockPath := filepath.Join(kubeletPluginsDir, entry.Name(), "csi.sock")
		if _, err := os.Stat(sockPath); err != nil {
			continue
		}

		// Try connecting and asking the driver its name
		conn, err := grpcConnect(sockPath)
		if err != nil {
			continue
		}
		identityClient := csi.NewIdentityClient(conn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		resp, err := identityClient.GetPluginInfo(ctx, &csi.GetPluginInfoRequest{})
		cancel()
		conn.Close()

		if err != nil {
			continue
		}
		if resp.GetName() == driverName {
			klog.Infof("Found socket for driver %s at %s", driverName, sockPath)
			return sockPath, nil
		}
	}

	return "", fmt.Errorf("no socket found for CSI driver %q in %s", driverName, kubeletPluginsDir)
}

func grpcConnect(socketPath string) (*grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := grpc.DialContext(ctx, "unix://"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return net.DialTimeout("unix", socketPath, 10*time.Second)
		}),
	)
	if err != nil {
		return nil, err
	}
	return conn, nil
}
