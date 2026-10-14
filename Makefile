.PHONY: build rootfs test vet

# A static binary: --init runs microbox inside the container, where the host's
# C library isn't available.
export CGO_ENABLED := 0

build:
	go build -o bin/microbox ./cmd/microbox

rootfs:
	sudo scripts/get-alpine.sh

vet:
	go vet ./...
	test -z "$$(gofmt -l .)"

# The e2e tests create namespaces and mounts, so they run as root.
# TESTFLAGS passes flags to go test, e.g. make test TESTFLAGS=-v.
test: vet
	sudo env "PATH=$$PATH" CGO_ENABLED=0 go test $(TESTFLAGS) ./...
