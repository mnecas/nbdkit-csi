package source

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/klog/v2"
)

// DiscoverStagedDevice finds the block device created by the source CSI driver
// after staging. Different drivers create devices in different locations.
func DiscoverStagedDevice(pvName, driverName string) (string, error) {
	// Wait a moment for the device to appear
	var device string
	var err error

	for i := 0; i < 30; i++ {
		device, err = findDevice(pvName, driverName)
		if err == nil && device != "" {
			return device, nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("device not found for PV %s after staging (driver: %s)", pvName, driverName)
}

func findDevice(pvName, driverName string) (string, error) {
	// Strategy 1: Check kubelet CSI plugin volume path
	// Some drivers (like Ceph) create a device symlink at:
	// /var/lib/kubelet/plugins/kubernetes.io/csi/pv/<pv-name>/globalmount
	globalMount := filepath.Join("/var/lib/kubelet/plugins/kubernetes.io/csi/pv", pvName, "globalmount")
	if info, err := os.Stat(globalMount); err == nil {
		if info.Mode()&os.ModeDevice != 0 || info.Mode()&os.ModeSymlink != 0 {
			klog.Infof("Found device at globalmount: %s", globalMount)
			return globalMount, nil
		}
		// Might be a directory with a device inside
		entries, err := os.ReadDir(globalMount)
		if err == nil {
			for _, e := range entries {
				path := filepath.Join(globalMount, e.Name())
				klog.V(4).Infof("Checking globalmount entry: %s", path)
				return path, nil
			}
		}
	}

	// Strategy 2: Check our own staging directory for a device or disk image file
	stagingDir := filepath.Join(StagingBaseDir, pvName)
	entries, err := os.ReadDir(stagingDir)
	if err == nil {
		// First pass: look for block devices
		for _, e := range entries {
			path := filepath.Join(stagingDir, e.Name())
			info, err := os.Stat(path)
			if err != nil {
				continue
			}
			if info.Mode()&os.ModeDevice != 0 {
				klog.Infof("Found device in staging dir: %s", path)
				return path, nil
			}
		}
		// Second pass: look for disk image files (for filesystem-backed volumes like NFS)
		diskFile := findDiskImage(stagingDir)
		if diskFile != "" {
			klog.Infof("Found disk image in staging dir: %s", diskFile)
			return diskFile, nil
		}
	}

	// Strategy 3: Check driver-specific paths
	device := findDriverSpecificDevice(pvName, driverName)
	if device != "" {
		return device, nil
	}

	// Strategy 4: Look for newly appeared /dev/rbd* devices
	device = findRBDDevice(pvName)
	if device != "" {
		return device, nil
	}

	return "", nil
}

func findDriverSpecificDevice(pvName, driverName string) string {
	// Ceph RBD: the driver maps the device and records it in:
	// /var/lib/kubelet/plugins/<driver>/node/<pv-name>/globalmount
	paths := []string{
		filepath.Join(kubeletPluginsDir, driverName, "node", pvName, "globalmount"),
		filepath.Join(kubeletPluginsDir, driverName, pvName, "globalmount"),
		filepath.Join(kubeletPluginsDir, driverName, "volumes", pvName),
	}

	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			if info.Mode()&os.ModeDevice != 0 {
				klog.Infof("Found device at driver path: %s", p)
				return p
			}
			// Check if it's a symlink to a device
			if info.Mode()&os.ModeSymlink != 0 {
				resolved, err := filepath.EvalSymlinks(p)
				if err == nil {
					klog.Infof("Found device via symlink: %s -> %s", p, resolved)
					return resolved
				}
			}
			// Could be a directory, look inside
			if info.IsDir() {
				entries, _ := os.ReadDir(p)
				for _, e := range entries {
					ep := filepath.Join(p, e.Name())
					klog.V(4).Infof("Checking driver path entry: %s", ep)
					if ei, err := os.Stat(ep); err == nil && ei.Mode()&os.ModeDevice != 0 {
						return ep
					}
				}
			}
		}
	}
	return ""
}

// findDiskImage searches a directory for a disk image file.
// Looks for well-known names and falls back to the largest regular file.
func findDiskImage(dir string) string {
	// Well-known disk image names (kubevirt, qemu, etc.)
	knownNames := []string{
		"disk.img",
		"disk.raw",
		"disk.qcow2",
		"image",
	}

	for _, name := range knownNames {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			return path
		}
	}

	// Fallback: find the largest regular file in the directory
	var largestPath string
	var largestSize int64

	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && path != dir {
			// Only go one level deep for common layouts
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.Size() > largestSize {
			largestSize = info.Size()
			largestPath = path
		}
		return nil
	})

	if largestSize > 0 {
		return largestPath
	}
	return ""
}

func findRBDDevice(pvName string) string {
	// Check /dev/rbd* devices and match via sysfs
	devDir := "/dev"
	entries, err := os.ReadDir(devDir)
	if err != nil {
		return ""
	}

	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "rbd") {
			continue
		}
		// Check sysfs for the RBD device name which might contain the PV info
		sysPath := filepath.Join("/sys/devices/rbd", strings.TrimPrefix(e.Name(), "rbd"), "name")
		data, err := os.ReadFile(sysPath)
		if err != nil {
			continue
		}
		imageName := strings.TrimSpace(string(data))
		if strings.Contains(imageName, pvName) || strings.Contains(pvName, imageName) {
			device := filepath.Join(devDir, e.Name())
			klog.Infof("Found RBD device: %s (image: %s)", device, imageName)
			return device
		}
	}
	return ""
}
