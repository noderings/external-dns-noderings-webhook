IMAGE ?= ghcr.io/noderings/external-dns-noderings-webhook
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint image

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/webhook ./cmd/webhook

test:
	go test -race ./...

lint:
	golangci-lint run

image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .
