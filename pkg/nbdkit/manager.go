package nbdkit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"k8s.io/klog/v2"
)

const (
	stateDir = "/var/lib/nbdkit-csi"
)

type VolumeState struct {
	VolumeID   string
	SocketPath string
	NBDDevice  string
	PID        int
	Config     *Config
	SourcePV   string
}

type Manager struct {
	mu      sync.RWMutex
	volumes map[string]*VolumeState
	nbd     *NBDDevicePool
}

func NewManager() (*Manager, error) {
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	pool, err := NewNBDDevicePool()
	if err != nil {
		return nil, fmt.Errorf("failed to create NBD device pool: %w", err)
	}

	return &Manager{
		volumes: make(map[string]*VolumeState),
		nbd:     pool,
	}, nil
}

func (m *Manager) GetVolume(volumeID string) *VolumeState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.volumes[volumeID]
}

func (m *Manager) StartNbdkit(volumeID string, cfg *Config, passwordFile string) (*VolumeState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if state, ok := m.volumes[volumeID]; ok {
		if isProcessRunning(state.PID) {
			klog.Infof("nbdkit already running for volume %s (pid %d)", volumeID, state.PID)
			return state, nil
		}
		klog.Warningf("stale nbdkit state for volume %s, cleaning up", volumeID)
		delete(m.volumes, volumeID)
	}

	volDir := filepath.Join(stateDir, volumeID)
	if err := os.MkdirAll(volDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create volume directory: %w", err)
	}

	socketPath := filepath.Join(volDir, "nbdkit.sock")

	args := m.buildNbdkitCommand(cfg, socketPath, passwordFile)

	klog.Infof("Starting nbdkit for volume %s: nbdkit %v", volumeID, args)

	cmd := exec.Command("nbdkit", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start nbdkit: %w", err)
	}

	// nbdkit with -U forks to background; wait for it
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("nbdkit exited with error: %w", err)
	}

	pid, err := readPidFile(filepath.Join(volDir, "nbdkit.pid"))
	if err != nil {
		return nil, fmt.Errorf("failed to read nbdkit pid: %w", err)
	}

	nbdDevice, err := m.nbd.Allocate()
	if err != nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return nil, fmt.Errorf("failed to allocate NBD device: %w", err)
	}

	if err := m.nbd.Connect(nbdDevice, socketPath); err != nil {
		m.nbd.Release(nbdDevice)
		_ = syscall.Kill(pid, syscall.SIGTERM)
		return nil, fmt.Errorf("failed to connect NBD device: %w", err)
	}

	state := &VolumeState{
		VolumeID:   volumeID,
		SocketPath: socketPath,
		NBDDevice:  nbdDevice,
		PID:        pid,
		Config:     cfg,
	}
	m.volumes[volumeID] = state

	klog.Infof("Volume %s staged: socket=%s, nbd=%s, pid=%d", volumeID, socketPath, nbdDevice, pid)
	return state, nil
}

func (m *Manager) StopNbdkit(volumeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	state, ok := m.volumes[volumeID]
	if !ok {
		klog.Infof("No nbdkit state for volume %s, nothing to stop", volumeID)
		return nil
	}

	if state.NBDDevice != "" {
		if err := m.nbd.Disconnect(state.NBDDevice); err != nil {
			klog.Warningf("Failed to disconnect NBD device %s: %v", state.NBDDevice, err)
		}
		m.nbd.Release(state.NBDDevice)
	}

	if state.PID > 0 && isProcessRunning(state.PID) {
		klog.Infof("Stopping nbdkit pid %d for volume %s", state.PID, volumeID)
		if err := syscall.Kill(state.PID, syscall.SIGTERM); err != nil {
			klog.Warningf("Failed to kill nbdkit pid %d: %v", state.PID, err)
		}
	}

	volDir := filepath.Join(stateDir, volumeID)
	_ = os.RemoveAll(volDir)

	delete(m.volumes, volumeID)
	return nil
}

func (m *Manager) GetNBDDevice(volumeID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if state, ok := m.volumes[volumeID]; ok {
		return state.NBDDevice
	}
	return ""
}

func (m *Manager) buildNbdkitCommand(cfg *Config, socketPath, passwordFile string) []string {
	args := []string{
		"-U", socketPath,
		"--pidfile", filepath.Join(filepath.Dir(socketPath), "nbdkit.pid"),
		"-f",  // don't fork (we manage it)
		"--exit-with-parent",
	}

	// Actually, use foreground=false so nbdkit daemonizes and we read the PID file.
	// Correct approach: let nbdkit fork, use pidfile.
	args = []string{
		"-U", socketPath,
		"--pidfile", filepath.Join(filepath.Dir(socketPath), "nbdkit.pid"),
	}

	for _, filter := range cfg.Filters {
		args = append(args, "--filter="+filter)
	}

	args = append(args, cfg.Plugin)
	args = append(args, cfg.PluginArgs...)

	if passwordFile != "" {
		args = append(args, "password=+"+passwordFile)
	}

	args = append(args, cfg.FilterArgs...)
	args = append(args, cfg.ExtraArgs...)

	return args
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func readPidFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0, fmt.Errorf("failed to parse pid from %q: %w", path, err)
	}
	return pid, nil
}
