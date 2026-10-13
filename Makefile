.PHONY: build rootfs test vet

build:
	go build -o bin/microbox ./cmd/microbox

rootfs:
	sudo scripts/get-alpine.sh

vet:
	go vet ./...
	test -z "$$(gofmt -l .)"

# The e2e tests create namespaces and mounts, so they run as root.
test: vet
	sudo env "PATH=$$PATH" go test ./...
