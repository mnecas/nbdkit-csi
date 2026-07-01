package nbdkit

import (
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

const (
	maxNBDDevices   = 128
	nbdDevicePrefix = "/dev/nbd"
)

type NBDDevicePool struct {
	mu       sync.Mutex
	inUse    map[string]bool
	maxDevs  int
}

func NewNBDDevicePool() (*NBDDevicePool, error) {
	if err := loadNBDModule(); err != nil {
		klog.Warningf("Failed to load nbd module (may already be loaded): %v", err)
	}

	return &NBDDevicePool{
		inUse:   make(map[string]bool),
		maxDevs: maxNBDDevices,
	}, nil
}

func (p *NBDDevicePool) Allocate() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i := 0; i < p.maxDevs; i++ {
		dev := fmt.Sprintf("%s%d", nbdDevicePrefix, i)
		if p.inUse[dev] {
			continue
		}
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		if isNBDDeviceFree(dev) {
			p.inUse[dev] = true
			return dev, nil
		}
	}
	return "", fmt.Errorf("no free NBD devices available (checked %d)", p.maxDevs)
}

func (p *NBDDevicePool) Release(device string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inUse, device)
}

func (p *NBDDevicePool) Connect(device, socketPath string) error {
	// Wait for the socket to appear
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	cmd := exec.Command("nbd-client", "-unix", socketPath, device, "-name", "")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nbd-client connect failed: %w, output: %s", err, string(output))
	}
	klog.Infof("Connected %s to socket %s", device, socketPath)
	return nil
}

func (p *NBDDevicePool) Disconnect(device string) error {
	cmd := exec.Command("nbd-client", "-d", device)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nbd-client disconnect failed: %w, output: %s", err, string(output))
	}
	klog.Infof("Disconnected %s", device)
	return nil
}

func loadNBDModule() error {
	cmd := exec.Command("modprobe", "nbd", fmt.Sprintf("nbds_max=%d", maxNBDDevices))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("modprobe nbd failed: %w, output: %s", err, string(output))
	}
	return nil
}

func isNBDDeviceFree(device string) bool {
	// Check if the device has a pid file in /sys/block/nbdX/pid
	sysPath := fmt.Sprintf("/sys/block/%s/pid", device[len("/dev/"):])
	_, err := os.ReadFile(sysPath)
	// If we can't read the pid, the device is free
	return err != nil
}
