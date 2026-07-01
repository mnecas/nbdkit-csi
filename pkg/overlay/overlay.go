package overlay

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mnecas/nbdkit-csi/pkg/nbdkit"
	"k8s.io/klog/v2"
)

const (
	kubeletCSIPath = "/var/lib/kubelet/plugins/kubernetes.io/csi"
	kubeletPodPath = "/var/lib/kubelet/pods"
)

// BuildOverlayConfig resolves the source PVC's device and wires it into the config.
func BuildOverlayConfig(cfg *nbdkit.Config, mgr *nbdkit.Manager) (*nbdkit.Config, error) {
	var sourceDevice string

	if cfg.SourceDevice != "" {
		sourceDevice = cfg.SourceDevice
	} else if cfg.SourcePV != "" {
		sourceDevice = resolveSourceDevice(cfg.SourcePV, mgr)
		if sourceDevice == "" {
			return nil, fmt.Errorf("source PV %q is not staged on this node or has no NBD device", cfg.SourcePV)
		}
	}

	klog.Infof("Resolved source to device %s", sourceDevice)
	nbdkit.ResolveSource(cfg, sourceDevice)
	cfg.SourcePV = ""
	cfg.SourceDevice = ""
	return cfg, nil
}

func resolveSourceDevice(sourcePVC string, mgr *nbdkit.Manager) string {
	// 1. Check if managed by this driver (internal state)
	device := mgr.GetNBDDevice(sourcePVC)
	if device != "" {
		return device
	}

	parts := strings.SplitN(sourcePVC, "/", 2)
	if len(parts) == 2 {
		device = mgr.GetNBDDevice(parts[1])
		if device != "" {
			return device
		}
	}

	// 2. Look up from kubelet's CSI staging paths (for volumes from other drivers)
	device = discoverFromKubeletPaths(sourcePVC)
	if device != "" {
		return device
	}

	return ""
}

// discoverFromKubeletPaths finds the block device for a PV staged by another CSI driver.
// Kubelet stages block volumes at:
//   /var/lib/kubelet/plugins/kubernetes.io/csi/pv/<pv-name>/globalmount
// and publishes them to pods at:
//   /var/lib/kubelet/pods/<pod-uid>/volumeDevices/kubernetes.io~csi/<pv-name>
func discoverFromKubeletPaths(name string) string {
	klog.Infof("Discovering source device for %q from kubelet paths", name)

	// Try the CSI global mount path directly with the given name
	globalMount := filepath.Join(kubeletCSIPath, "pv", name, "globalmount")
	klog.V(4).Infof("Checking globalmount path: %s", globalMount)
	if device, err := resolveBlockDevice(globalMount); err == nil {
		klog.Infof("Found source device via globalmount: %s -> %s", globalMount, device)
		return device
	}

	// Scan all PV directories under kubelet CSI path for a match
	pvDir := filepath.Join(kubeletCSIPath, "pv")
	entries, err := os.ReadDir(pvDir)
	if err == nil {
		klog.V(4).Infof("Scanning %d PV entries in %s", len(entries), pvDir)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			// Check if the PV directory name contains the source name
			if strings.Contains(entry.Name(), name) {
				candidate := filepath.Join(pvDir, entry.Name(), "globalmount")
				if device, err := resolveBlockDevice(candidate); err == nil {
					klog.Infof("Found source device via PV scan: %s -> %s", candidate, device)
					return device
				}
			}
		}
	} else {
		klog.V(4).Infof("Cannot read PV dir %s: %v", pvDir, err)
	}

	// Try finding published volume devices in pod directories
	device := findPublishedVolumeDevice(name)
	if device != "" {
		return device
	}

	return ""
}

// resolveBlockDevice checks if path is a block device or a symlink to one.
func resolveBlockDevice(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}

	// If it's a symlink, resolve it
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

	// If the path itself exists and is a device (stat shows block device)
	if info.Mode()&os.ModeDevice != 0 {
		return path, nil
	}

	// The path exists as a regular file -- it may be a bind-mounted block device
	return path, nil
}

// findPublishedVolumeDevice searches pod volume device paths for a matching PV name.
func findPublishedVolumeDevice(name string) string {
	podDirs, err := os.ReadDir(kubeletPodPath)
	if err != nil {
		klog.V(4).Infof("Cannot read kubelet pods dir: %v", err)
		return ""
	}

	for _, podDir := range podDirs {
		if !podDir.IsDir() {
			continue
		}

		// Check volumeDevices (block volumes)
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

		// Check volumes (filesystem mounts) - the mount source might be a block device
		volMountsDir := filepath.Join(kubeletPodPath, podDir.Name(), "volumes", "kubernetes.io~csi")
		mountEntries, err := os.ReadDir(volMountsDir)
		if err != nil {
			continue
		}
		for _, mountEntry := range mountEntries {
			if mountEntry.Name() == name || strings.Contains(mountEntry.Name(), name) {
				mountPath := filepath.Join(volMountsDir, mountEntry.Name(), "mount")
				if _, err := os.Stat(mountPath); err == nil {
					klog.Infof("Found source mount via pod volume: %s", mountPath)
					return mountPath
				}
			}
		}
	}
	return ""
}
