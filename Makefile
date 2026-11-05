GO       ?= go
export GOFLAGS ?= -mod=vendor
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE     ?= $(shell date -u +%Y-%m-%d)
PKG      := github.com/xiaoguan521/edgetraffic-os/internal/version
LDFLAGS  := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).BuildDate=$(DATE)

.PHONY: all build console agent wrapper license-server test test-imports test-import-ui lint lint-web brand web proto clean gen gen-check

all: test build

build: console agent wrapper license-server

console agent license-server:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/syapp-$@ ./cmd/syapp-$@

wrapper:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/syapp-wrapper ./cmd/syapp-wrapper

test:
	$(GO) vet ./...
	$(GO) test -race -count=1 ./...

# Real HTTP routing/middleware/database tests; ETOS_TEST_DB selects PostgreSQL.
test-imports:
	$(GO) test -race -count=1 -run TestImportHTTP ./internal/console/api

# Requires a disposable initialized console and test signer; see test/playwright/README.md.
IMPORT_REPORT_DIR ?= dist/test-reports/import-ui
test-import-ui:
	mkdir -p "$(IMPORT_REPORT_DIR)"
	node --test --test-reporter=spec --test-reporter=junit --test-reporter-destination=stdout --test-reporter-destination="$(IMPORT_REPORT_DIR)/junit.xml" web/e2e/import-acceptance.cjs

# 由 model.go 重新生成 gorm/gen 查询代码（控制台与授权后台的 store/query），结果需提交
GEN_DIRS := internal/console/store/query internal/portal/store/query
gen:
	$(GO) generate ./internal/console/store/ ./internal/portal/store/

# CI：生成代码必须与模型同步
gen-check: gen
	git diff --exit-code -- $(GEN_DIRS) || { echo "生成代码已过期：执行 make gen 并提交"; exit 1; }
	test -z "$$(git status --porcelain -- $(GEN_DIRS))" || { echo "有未提交的生成文件：执行 make gen 并提交"; exit 1; }

lint:
	@unfmt="$$(gofmt -l cmd internal dataplane)"; if [ -n "$$unfmt" ]; then echo "gofmt needed:"; echo "$$unfmt"; exit 1; fi
	golangci-lint run ./...

lint-web:
	cd web && pnpm lint

brand:
	./scripts/brand-check.sh

web:
	cd web && pnpm install && pnpm build && touch dist/.gitkeep portal/dist/.gitkeep

proto:
	cd api/proto && buf generate

syapp:
	@echo "usage: dataplane/build-syapp.sh <upstream.tar.gz> [openssl|tongsuo]"

clean:
	rm -rf bin web/dist web/portal/dist/assets web/portal/dist/index.html
