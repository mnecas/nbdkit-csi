FROM golang:1.26 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o /nbdkit-csi ./cmd/nbdkit-csi/

FROM fedora:42

RUN dnf install -y \
    nbdkit-server \
    nbdkit-basic-plugins \
    nbdkit-basic-filters \
    nbdkit-vddk-plugin \
    nbdkit-nbd-plugin \
    nbdkit-curl-plugin \
    nbdkit-guestfs-plugin \
    nbdkit-ssh-plugin \
    nbdkit-iso-plugin \
    nbdkit-stats-filter \
    nbdkit-xz-filter \
    nbd \
    e2fsprogs \
    xfsprogs \
    util-linux \
    kmod \
    && dnf clean all

COPY --from=builder /nbdkit-csi /usr/local/bin/nbdkit-csi

ENTRYPOINT ["/usr/local/bin/nbdkit-csi"]
