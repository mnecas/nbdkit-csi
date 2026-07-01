package overlay

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mnecas/nbdkit-csi/pkg/nbdkit"
	"github.com/mnecas/nbdkit-csi/pkg/source"
	"k8s.io/klog/v2"
)

const (
	kubeletCSIPath = "/var/lib/kubelet/plugins/kubernetes.io/csi"
	kubeletPodPath = "/var/lib/kubelet/pods"
)

// BuildOverlayConfig resolves the source PVC's device and wires it into the config.
// It first tries to discover an already-staged device, and if not found, stages
// the source volume by calling the source CSI driver directly.
func BuildOverlayConfig(ctx context.Context, cfg *nbdkit.Config, mgr *nbdkit.Manager, stager *source.SourceStager, nodeID string) (*nbdkit.Config, error) {
	var sourceDevice string

	if cfg.SourceDevice != "" {
		sourceDevice = cfg.SourceDevice
	} else if cfg.SourcePV != "" {
		// Try to find an already-staged device first
		sourceDevice = discoverExistingDevice(cfg.SourcePV, mgr)

		// If not found, stage the source volume ourselves
		if sourceDevice == "" {
			klog.Infof("Source PV %q not found on node, staging it via source CSI driver", cfg.SourcePV)
			_, driverName, err := stager.StageSourceVolume(ctx, cfg.SourcePV, nodeID)
			if err != nil {
				return nil, fmt.Errorf("failed to stage source volume: %w", err)
			}

			// Discover the device the source driver created
			sourceDevice, err = source.DiscoverStagedDevice(cfg.SourcePV, driverName)
			if err != nil {
				return nil, fmt.Errorf("failed to discover staged device: %w", err)
			}
		}
	}

	if sourceDevice == "" {
		return nil, fmt.Errorf("could not resolve source device")
	}

	klog.Infof("Resolved source to device %s", sourceDevice)
	nbdkit.ResolveSource(cfg, sourceDevice)
	cfg.SourcePV = ""
	cfg.SourceDevice = ""
	return cfg, nil
}

// UnstageSource unstages the source volume if we staged it.
func UnstageSource(ctx context.Context, pvName string, stager *source.SourceStager) {
	if pvName == "" {
		return
	}
	if err := stager.UnstageSourceVolume(ctx, pvName); err != nil {
		klog.Warningf("Failed to unstage source volume %s: %v", pvName, err)
	}
}

func discoverExistingDevice(pvName string, mgr *nbdkit.Manager) string {
	// 1. Check internal nbdkit-csi state
	device := mgr.GetNBDDevice(pvName)
	if device != "" {
		return device
	}

	// 2. Check kubelet paths
	device = discoverFromKubeletPaths(pvName)
	if device != "" {
		return device
	}

	return ""
}

func discoverFromKubeletPaths(name string) string {
	klog.V(4).Infof("Checking kubelet paths for %q", name)

	// Try the CSI global mount path
	globalMount := filepath.Join(kubeletCSIPath, "pv", name, "globalmount")
	if device, err := resolveBlockDevice(globalMount); err == nil {
		klog.Infof("Found source device via globalmount: %s -> %s", globalMount, device)
		return device
	}

	// Scan PV directories
	pvDir := filepath.Join(kubeletCSIPath, "pv")
	entries, err := os.ReadDir(pvDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if strings.Contains(entry.Name(), name) {
				candidate := filepath.Join(pvDir, entry.Name(), "globalmount")
				if device, err := resolveBlockDevice(candidate); err == nil {
					klog.Infof("Found source device via PV scan: %s -> %s", candidate, device)
					return device
				}
			}
		}
	}

	// Try pod volume devices
	device := findPublishedVolumeDevice(name)
	if device != "" {
		return device
	}

	return ""
}

func resolveBlockDevice(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(path), resolved)
		}
		return resolved, nil
	}

	if info.Mode()&os.ModeDevice != 0 {
		return path, nil
	}

	return path, nil
}

func findPublishedVolumeDevice(name string) string {
	podDirs, err := os.ReadDir(kubeletPodPath)
	if err != nil {
		return ""
	}

	for _, podDir := range podDirs {
		if !podDir.IsDir() {
			continue
		}

		volDevicesDir := filepath.Join(kubeletPodPath, podDir.Name(), "volumeDevices", "kubernetes.io~csi")
		volEntries, err := os.ReadDir(volDevicesDir)
		if err != nil {
			continue
		}
		for _, volEntry := range volEntries {
			if volEntry.Name() == name || strings.Contains(volEntry.Name(), name) {
				devicePath := filepath.Join(volDevicesDir, volEntry.Name())
				if device, err := resolveBlockDevice(devicePath); err == nil {
					klog.Infof("Found source device via pod volume: %s -> %s", devicePath, device)
					return device
				}
			}
		}
	}
	return ""
}
