# nbdkit-csi

A Kubernetes CSI driver that uses [nbdkit](https://gitlab.com/nbdkit/nbdkit) to expose remote disks as Persistent Volumes. Supports any nbdkit plugin and filter combination, including VDDK connections to VMware and overlay volumes with delay/COW filters.

## Features

- **No dynamic provisioning** -- users create PVs with nbdkit parameters, the driver manages nbdkit processes on nodes
- **Any nbdkit plugin** -- VDDK, curl, file, nbd, ssh, guestfs, iso, S3, etc.
- **Any nbdkit filter** -- delay, cow, cache, rate, readahead, log, etc.
- **Overlay mode** -- layer filters on top of an existing PVC's device
- **Block and filesystem volumes** -- expose NBD devices as raw block or formatted filesystems

## Prerequisites

- Kubernetes 1.26+ or OpenShift 4.x
- Nodes with `nbd` kernel module available
- For VDDK: VMware VDDK library installed on nodes (or mounted into the container)

## Installation

### Build

```bash
make image   # builds quay.io/mnecas/nbdkit-csi:latest
make push    # pushes to quay.io
```

### Deploy on OpenShift

```bash
oc apply -f deploy/kubernetes/namespace.yaml
oc apply -f deploy/kubernetes/scc.yaml
oc apply -f deploy/kubernetes/rbac.yaml
oc apply -f deploy/kubernetes/csidriver.yaml
oc apply -f deploy/kubernetes/daemonset.yaml
```

### Deploy on Kubernetes

```bash
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl apply -f deploy/kubernetes/rbac.yaml
kubectl apply -f deploy/kubernetes/csidriver.yaml
kubectl apply -f deploy/kubernetes/daemonset.yaml
```

### Verify

```bash
kubectl -n nbdkit-csi get pods
kubectl get csidriver nbdkit.csi.k8s.io
```

## Usage

### VDDK Volume (VMware remote disk)

1. Create a secret with the ESXi/vCenter password:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: vddk-credentials
type: Opaque
stringData:
  password: "your-password"
```

2. Create a PV with plugin parameters:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: vmware-disk-1
spec:
  capacity:
    storage: 100Gi
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: nbdkit.csi.k8s.io
    volumeHandle: vmware-disk-1
    volumeAttributes:
      nbdkit.csi.k8s.io/plugin: "vddk"
      nbdkit.csi.k8s.io/param-server: "esxi.example.com"
      nbdkit.csi.k8s.io/param-user: "root"
      nbdkit.csi.k8s.io/param-vm: "moref=2"
      nbdkit.csi.k8s.io/param-file: "[datastore1] VM/disk.vmdk"
      nbdkit.csi.k8s.io/param-thumbprint: "AA:BB:CC:..."
      nbdkit.csi.k8s.io/param-libdir: "/opt/vmware-vix-disklib-distrib"
    nodeStageSecretRef:
      name: vddk-credentials
      namespace: default
```

3. Create a PVC bound to the PV and use it in a pod.

### Overlay with Delay Filter

Create a volume that sits on top of an existing staged volume and adds I/O delays:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: delayed-disk
spec:
  capacity:
    storage: 100Gi
  accessModes: [ReadWriteOnce]
  volumeMode: Block
  persistentVolumeReclaimPolicy: Retain
  csi:
    driver: nbdkit.csi.k8s.io
    volumeHandle: delayed-disk-1
    volumeAttributes:
      nbdkit.csi.k8s.io/source-pvc: "vmware-disk-1"
      nbdkit.csi.k8s.io/filters: "delay,cow"
      nbdkit.csi.k8s.io/filter-param-delay-read: "100ms"
      nbdkit.csi.k8s.io/filter-param-delay-write: "200ms"
      nbdkit.csi.k8s.io/filter-param-delay-zero: "50ms"
```

The `source-pvc` value references the `volumeHandle` of the source PV, which must already be staged on the same node.

### Overlay with Custom Plugin

Use `{source}` as a placeholder for the resolved device path when combining `source-pvc` with a custom plugin:

```yaml
volumeAttributes:
  nbdkit.csi.k8s.io/source-pvc: "vmware-disk-1"
  nbdkit.csi.k8s.io/plugin: "guestfs"
  nbdkit.csi.k8s.io/param-disk: "{source}"
  nbdkit.csi.k8s.io/param-export: "/dev/sda1"
```

### Other Plugins

Any nbdkit plugin works the same way:

```yaml
# curl plugin
volumeAttributes:
  nbdkit.csi.k8s.io/plugin: "curl"
  nbdkit.csi.k8s.io/param-url: "https://example.com/disk.raw"
  nbdkit.csi.k8s.io/filters: "readahead,cache"

# ssh plugin
volumeAttributes:
  nbdkit.csi.k8s.io/plugin: "ssh"
  nbdkit.csi.k8s.io/param-host: "server.example.com"
  nbdkit.csi.k8s.io/param-path: "/var/lib/images/disk.img"

# nbd plugin (connect to another NBD server)
volumeAttributes:
  nbdkit.csi.k8s.io/plugin: "nbd"
  nbdkit.csi.k8s.io/param-hostname: "nbd-server.local"
  nbdkit.csi.k8s.io/param-port: "10809"
  nbdkit.csi.k8s.io/filters: "rate"
  nbdkit.csi.k8s.io/filter-param-rate: "50M"

# iso plugin
volumeAttributes:
  nbdkit.csi.k8s.io/plugin: "iso"
  nbdkit.csi.k8s.io/param-dir: "/path/to/directory"
  nbdkit.csi.k8s.io/param-params: "-JrT"
```

## Volume Attributes Reference

| Attribute | Description |
|-----------|-------------|
| `nbdkit.csi.k8s.io/plugin` | nbdkit plugin name (required unless `source-pvc` is set) |
| `nbdkit.csi.k8s.io/source-pvc` | Source volume handle to use as backing device |
| `nbdkit.csi.k8s.io/filters` | Comma-separated list of filters to apply |
| `nbdkit.csi.k8s.io/param-<key>` | Plugin argument passed as `key=value` to nbdkit |
| `nbdkit.csi.k8s.io/filter-param-<key>` | Filter argument passed as `key=value` to nbdkit |
| `nbdkit.csi.k8s.io/extra-args` | Additional raw space-separated nbdkit CLI arguments |

When `source-pvc` is set, the resolved device path replaces any `{source}` placeholder in param values. If no placeholder is found, the device is appended as a positional argument (default behavior for the `file` plugin).

## How It Works

1. User creates a PV with CSI driver `nbdkit.csi.k8s.io` and volume attributes
2. When a pod mounts the PVC, kubelet calls `NodeStageVolume`
3. The driver starts an nbdkit process with the configured plugin and filters, listening on a Unix socket
4. The driver connects `/dev/nbdX` to the socket using `nbd-client`
5. On `NodePublishVolume`, the device is bind-mounted (block) or formatted and mounted (filesystem) into the pod
6. On teardown, the mount is removed, NBD disconnected, and nbdkit process stopped

## Development

```bash
make build     # Build binary
make test      # Run tests
make image     # Build container image
make push      # Build and push to registry
make lint      # Format and vet
```

## License

Apache-2.0
