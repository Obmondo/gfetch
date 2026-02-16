IMAGE_REGISTRY := ghcr.io/obmondo
IMAGE_NAME := gfetch
VERSION := $(shell git describe --tags 2>/dev/null || echo "dev")
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')

BINARY := gfetch
LDFLAGS := -s -w \
	-X github.com/ashish1099/gfetch/internal/cli.Version=$(VERSION) \
	-X github.com/ashish1099/gfetch/internal/cli.Commit=$(COMMIT) \
	-X github.com/ashish1099/gfetch/internal/cli.Date=$(DATE)

.PHONY: build test docker-build docker-push lint clean

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/gfetch

test:
	go test ./...

docker-build:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) \
		-t $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(VERSION) \
		-t $(IMAGE_REGISTRY)/$(IMAGE_NAME):latest \
		.

docker-push:
	docker push $(IMAGE_REGISTRY)/$(IMAGE_NAME):$(VERSION)
	docker push $(IMAGE_REGISTRY)/$(IMAGE_NAME):latest

lint:
	golangci-lint run

clean:
	rm -f $(BINARY)
	rm -rf dist/
