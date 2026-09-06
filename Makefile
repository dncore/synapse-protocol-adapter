BINARY := protocol-proxy
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build build-darwin test race bench vet clean docker load load-stream install

build:
	CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o $(BINARY) ./cmd/proxy

# Cross-compile for macOS (arm64 by default; make build-darwin DARWIN_ARCH=amd64 for Intel).
DARWIN_ARCH ?= arm64
build-darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=$(DARWIN_ARCH) go build -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o $(BINARY)-darwin-$(DARWIN_ARCH) ./cmd/proxy

test:
	go test ./...

race:
	go test -race ./...

bench:
	go test -bench=. -benchtime=2s ./internal/converter/

vet:
	go vet ./...

docker:
	docker build -t protocol-proxy:$(VERSION) -t protocol-proxy:latest .

# End-to-end load tests against a locally running proxy.
load:
	go run ./tests/load -url http://127.0.0.1:8787/v1/responses -concurrency 100 -duration 15s

load-stream:
	go run ./tests/load -url http://127.0.0.1:8787/v1/responses -concurrency 100 -duration 15s -stream

install: build
	install -Dm755 $(BINARY) /usr/local/bin/$(BINARY)

clean:
	rm -f $(BINARY)
