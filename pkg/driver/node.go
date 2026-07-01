package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mnecas/nbdkit-csi/pkg/nbdkit"
	"github.com/mnecas/nbdkit-csi/pkg/overlay"
	"github.com/mnecas/nbdkit-csi/pkg/source"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

type NodeServer struct {
	csi.UnimplementedNodeServer
	nodeID  string
	manager *nbdkit.Manager
	stager  *source.SourceStager
}

func (s *NodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: s.nodeID,
	}, nil
}

func (s *NodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (s *NodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.GetStagingTargetPath() == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	attrs := req.GetVolumeContext()
	cfg, err := nbdkit.ParseVolumeAttributes(attrs)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse volume attributes: %v", err)
	}

	var passwordFile string
	var sourcePV string
	if nbdkit.IsOverlay(cfg) {
		sourcePV = cfg.SourcePV
		overlayCfg, err := overlay.BuildOverlayConfig(ctx, cfg, s.manager, s.stager, s.nodeID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to build overlay config: %v", err)
		}
		cfg = overlayCfg
	} else {
		passwordFile, err = writePasswordFile(volumeID, req.GetSecrets())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "failed to write password file: %v", err)
		}
	}

	state, err := s.manager.StartNbdkit(volumeID, cfg, passwordFile)
	if err != nil {
		cleanupPasswordFile(volumeID)
		return nil, status.Errorf(codes.Internal, "failed to start nbdkit: %v", err)
	}
	state.SourcePV = sourcePV

	klog.Infof("NodeStageVolume succeeded for volume %s", volumeID)
	return &csi.NodeStageVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	// Get the volume state to check if it has a source PV to unstage
	volState := s.manager.GetVolume(volumeID)

	if err := s.manager.StopNbdkit(volumeID); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to stop nbdkit: %v", err)
	}

	// Unstage source volume if we staged it
	if volState != nil && volState.SourcePV != "" {
		overlay.UnstageSource(ctx, volState.SourcePV, s.stager)
	}

	cleanupPasswordFile(volumeID)
	klog.Infof("NodeUnstageVolume succeeded for volume %s", volumeID)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (s *NodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	nbdDevice := s.manager.GetNBDDevice(volumeID)
	if nbdDevice == "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s not staged", volumeID)
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	switch volCap.GetAccessType().(type) {
	case *csi.VolumeCapability_Block:
		if err := publishBlockVolume(nbdDevice, targetPath); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to publish block volume: %v", err)
		}
	case *csi.VolumeCapability_Mount:
		mountInfo := volCap.GetMount()
		fsType := mountInfo.GetFsType()
		if fsType == "" {
			fsType = "ext4"
		}
		mountFlags := mountInfo.GetMountFlags()
		if err := publishMountVolume(nbdDevice, targetPath, fsType, mountFlags); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to publish mount volume: %v", err)
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported volume access type")
	}

	klog.Infof("NodePublishVolume succeeded for volume %s at %s", volumeID, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	if err := unmount(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount %s: %v", targetPath, err)
	}

	klog.Infof("NodeUnpublishVolume succeeded for volume %s", volumeID)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func publishBlockVolume(device, targetPath string) error {
	if err := os.MkdirAll(filepath.Dir(targetPath), 0750); err != nil {
		return fmt.Errorf("failed to create parent directory: %w", err)
	}

	f, err := os.OpenFile(targetPath, os.O_CREATE, 0660)
	if err != nil {
		return fmt.Errorf("failed to create target file: %w", err)
	}
	f.Close()

	cmd := exec.Command("mount", "--bind", device, targetPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bind mount failed: %w, output: %s", err, string(output))
	}
	return nil
}

func publishMountVolume(device, targetPath, fsType string, mountFlags []string) error {
	// Format the device if needed
	if needsFormat(device, fsType) {
		klog.Infof("Formatting %s as %s", device, fsType)
		cmd := exec.Command(fmt.Sprintf("mkfs.%s", fsType), device)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("mkfs failed: %w, output: %s", err, string(output))
		}
	}

	if err := os.MkdirAll(targetPath, 0750); err != nil {
		return fmt.Errorf("failed to create target directory: %w", err)
	}

	args := []string{"-t", fsType}
	if len(mountFlags) > 0 {
		args = append(args, "-o")
		flags := ""
		for i, f := range mountFlags {
			if i > 0 {
				flags += ","
			}
			flags += f
		}
		args = append(args, flags)
	}
	args = append(args, device, targetPath)

	cmd := exec.Command("mount", args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount failed: %w, output: %s", err, string(output))
	}
	return nil
}

func unmount(targetPath string) error {
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return nil
	}

	cmd := exec.Command("umount", targetPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		// If already unmounted, not an error
		klog.Warningf("umount %s: %v, output: %s", targetPath, err, string(output))
	}

	_ = os.Remove(targetPath)
	return nil
}

func needsFormat(device, fsType string) bool {
	cmd := exec.Command("blkid", "-p", "-s", "TYPE", "-o", "value", device)
	output, err := cmd.Output()
	if err != nil {
		return true
	}
	return len(output) == 0
}
