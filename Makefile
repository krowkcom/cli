# The leading v comes off, because GoReleaser drops it and npm will not take it.
# A checkout and a release should not disagree about what version this is.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -s -w -X github.com/krowkcom/cli/internal/cli.Version=$(VERSION)

.PHONY: build test lint vet fmt check mock install clean dist release-check

build: ## Build ./bin/krowk and ./bin/krowk-mcp
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/krowk ./cmd/krowk
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/krowk-mcp ./cmd/krowk-mcp

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint: ## Requires golangci-lint; falls back to vet
	@command -v golangci-lint >/dev/null && golangci-lint run || $(MAKE) vet

check: vet test ## Everything CI runs

mock: ## Local stand-in for api.krowk.com on :8787
	go run ./cmd/krowk registry serve

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/krowk ./cmd/krowk-mcp

release-check: ## Validate .goreleaser.yaml and the npm launchers, offline
	goreleaser check
	node --check npm/krowk/bin/krowk.js
	node --check npm/mcp/bin/krowk-mcp.js

dist: ## The whole release, locally: every binary, the archives, the npm packages
	goreleaser release --snapshot --clean --skip=publish
	node npm/build.mjs

clean:
	rm -rf bin dist
