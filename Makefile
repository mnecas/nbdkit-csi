BINARY_NAME := nbdkit-csi
IMAGE_NAME := quay.io/mnecas0/nbdkit-csi
IMAGE_TAG := latest
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: all build clean test image push

all: build

build:
	go build $(LDFLAGS) -o bin/$(BINARY_NAME) ./cmd/nbdkit-csi/

clean:
	rm -rf bin/

test:
	go test ./... -v

image:
	podman build -t $(IMAGE_NAME):$(IMAGE_TAG) .

push: image
	podman push $(IMAGE_NAME):$(IMAGE_TAG)

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: fmt vet
