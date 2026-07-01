package driver

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/mnecas/nbdkit-csi/pkg/nbdkit"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

const (
	DriverName = "nbdkit.csi.k8s.io"
)

type Driver struct {
	nodeID   string
	endpoint string
	version  string

	manager *nbdkit.Manager

	srv *grpc.Server
	mu  sync.Mutex
}

func New(nodeID, endpoint, version string) (*Driver, error) {
	mgr, err := nbdkit.NewManager()
	if err != nil {
		return nil, fmt.Errorf("failed to create nbdkit manager: %w", err)
	}

	return &Driver{
		nodeID:   nodeID,
		endpoint: endpoint,
		version:  version,
		manager:  mgr,
	}, nil
}

func (d *Driver) Run() error {
	u, err := url.Parse(d.endpoint)
	if err != nil {
		return fmt.Errorf("failed to parse endpoint %q: %w", d.endpoint, err)
	}

	if u.Scheme != "unix" {
		return fmt.Errorf("only unix domain sockets are supported, got: %s", u.Scheme)
	}

	socketPath := u.Path
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket %q: %w", socketPath, err)
	}

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on %q: %w", socketPath, err)
	}

	d.srv = grpc.NewServer(grpc.UnaryInterceptor(logInterceptor))

	csi.RegisterIdentityServer(d.srv, &IdentityServer{
		name:    DriverName,
		version: d.version,
	})

	csi.RegisterNodeServer(d.srv, &NodeServer{
		nodeID:  d.nodeID,
		manager: d.manager,
	})

	csi.RegisterControllerServer(d.srv, &ControllerServer{})

	klog.Infof("Listening on %s", socketPath)
	return d.srv.Serve(listener)
}

func (d *Driver) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.srv != nil {
		d.srv.GracefulStop()
	}
}
