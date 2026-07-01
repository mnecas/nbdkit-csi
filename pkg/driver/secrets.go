package driver

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"
)

const (
	secretsDir  = "/var/lib/nbdkit-csi/secrets"
	passwordKey = "password"
)

func writePasswordFile(volumeID string, secrets map[string]string) (string, error) {
	password, ok := secrets[passwordKey]
	if !ok || password == "" {
		return "", nil
	}

	dir := filepath.Join(secretsDir, volumeID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("failed to create secrets directory: %w", err)
	}

	path := filepath.Join(dir, "password")
	if err := os.WriteFile(path, []byte(password), 0600); err != nil {
		return "", fmt.Errorf("failed to write password file: %w", err)
	}

	klog.V(4).Infof("Wrote password file for volume %s", volumeID)
	return path, nil
}

func cleanupPasswordFile(volumeID string) {
	dir := filepath.Join(secretsDir, volumeID)
	if err := os.RemoveAll(dir); err != nil {
		klog.Warningf("Failed to cleanup secrets for volume %s: %v", volumeID, err)
	}
}
