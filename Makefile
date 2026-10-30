GO       ?= go
export GOFLAGS ?= -mod=vendor
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%d)
PKG      := github.com/xiaoguan521/edgetraffic-os/internal/version
LDFLAGS  := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildDate=$(DATE)

.PHONY: all build console agent wrapper license-server test lint brand web proto clean gen gen-check

all: test build

build: console agent wrapper license-server

console agent license-server:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/syapp-$@ ./cmd/syapp-$@

wrapper:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/syapp-wrapper ./cmd/syapp-wrapper

test:
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

# 由 model.go 重新生成 gorm/gen 查询代码（internal/console/store/query），结果需提交
gen:
	$(GO) generate ./internal/console/store/

# CI：生成代码必须与模型同步
gen-check: gen
	git diff --exit-code -- internal/console/store/query || { echo "生成代码已过期：执行 make gen 并提交"; exit 1; }
	test -z "$$(git status --porcelain -- internal/console/store/query)" || { echo "有未提交的生成文件：执行 make gen 并提交"; exit 1; }

lint:
	golangci-lint run ./...

brand:
	./scripts/brand-check.sh

web:
	cd web && pnpm install && pnpm build && touch dist/.gitkeep

proto:
	cd api/proto && buf generate

syapp:
	@echo "usage: dataplane/build-syapp.sh <upstream.tar.gz> [openssl|tongsuo]"

clean:
	rm -rf bin web/dist
