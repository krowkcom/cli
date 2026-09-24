# The leading v comes off, because GoReleaser drops it and npm will not take it.
# A checkout and a release should not disagree about what version this is.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS := -s -w -X github.com/krowkcom/cli/internal/cli.Version=$(VERSION)

.PHONY: build test lint vet fmt check windows-build mock install clean dist release-check rust golden golden-rust golden-update

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

# The Windows build is compile-only and on purpose: `sessions` refuses to run
# there, but the package still has to compile, and the build-tagged halves of
# the import lock are only ever checked by a cross build. A test run on Linux
# never touches importlock_windows.go.
windows-build:
	GOOS=windows go build ./...

check: vet windows-build test ## Everything CI runs

mock: ## Local stand-in for api.krowk.com on :8787
	go run ./internal/devregistry

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/krowk ./cmd/krowk-mcp

release-check: ## Validate .goreleaser.yaml, the npm launchers and the installer, offline
	goreleaser check
	node --check npm/krowk/bin/krowk.js
	node --check npm/mcp/bin/krowk-mcp.js
	# The installer downloads what this file produces, so it belongs to the
	# release pipeline rather than to `check`: it needs goreleaser and python3,
	# which a plain `go test` run has no business requiring.
	scripts/install_test.sh

dist: ## The whole release, locally: every binary, the archives, the npm packages
	goreleaser release --snapshot --clean --skip=publish
	node npm/build.mjs

clean:
	rm -rf bin dist

# The Rust port (Cargo.toml). Go above is still what ships; these targets are
# how the port proves it can take over.
rust: ## Build the Rust krowk into target/release
	cargo build --release -p krowk

golden: build ## Hold the Go build to tests/golden/cases
	cargo test -p krowk-golden

golden-rust: rust ## Hold the Rust build to the same cases
	KROWK_BIN=target/release/krowk cargo test -p krowk-golden

golden-update: build ## Re-record the cases from the Go build
	GOLDEN_UPDATE=1 cargo test -p krowk-golden
