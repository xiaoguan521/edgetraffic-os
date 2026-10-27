GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%d)
PKG      := github.com/xiaoguan521/edgetraffic-os/internal/version
LDFLAGS  := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildDate=$(DATE)

.PHONY: all build console agent wrapper license-server test lint brand web proto clean

all: test build

build: console agent wrapper license-server

console agent license-server:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/syapp-$@ ./cmd/syapp-$@

wrapper:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/syapp-wrapper ./cmd/syapp-wrapper

test:
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

lint:
	golangci-lint run ./...

brand:
	./scripts/brand-check.sh

web:
	cd web && pnpm install && pnpm build

proto:
	cd api/proto && buf generate

syapp:
	@echo "usage: dataplane/build-syapp.sh <upstream.tar.gz> [openssl|tongsuo]"

clean:
	rm -rf bin web/dist
