package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/mnecas/nbdkit-csi/pkg/driver"
	"k8s.io/klog/v2"
)

var (
	endpoint = flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	nodeID   = flag.String("node-id", "", "Node ID (defaults to hostname)")
	version  = "dev"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	if *nodeID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get hostname: %v\n", err)
			os.Exit(1)
		}
		*nodeID = hostname
	}

	klog.Infof("nbdkit-csi driver version %s starting", version)
	klog.Infof("Node ID: %s", *nodeID)
	klog.Infof("Endpoint: %s", *endpoint)

	drv, err := driver.New(*nodeID, *endpoint, version)
	if err != nil {
		klog.Fatalf("Failed to create driver: %v", err)
	}

	if err := drv.Run(); err != nil {
		klog.Fatalf("Failed to run driver: %v", err)
	}
}
