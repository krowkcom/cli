# The leading v comes off, because the release drops it and npm will not take it.
# A checkout and a release should not disagree about what version this is.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)

.PHONY: build test lint check install mock clean dist release-check golden golden-update bin/devregistry

build: ## Build target/release/krowk (with sessions) and krowk-mcp
	KROWK_VERSION=$(VERSION) cargo build --release -p krowk --features sessions

test: ## The unit and integration tests
	cargo test --workspace --exclude krowk-golden --features krowk/sessions

# Both builds: the agent build (no sessions) is the one a container compiles
# from source, and a cfg that only one of them sees is a lint only one catches.
lint:
	cargo clippy --workspace --all-targets -- -D warnings
	cargo clippy --workspace --all-targets --features krowk/sessions -- -D warnings

check: lint test golden ## Everything CI runs

install: ## Install krowk and krowk-mcp into ~/.cargo/bin
	KROWK_VERSION=$(VERSION) cargo install --locked --path crates/krowk --features sessions

mock: ## Local stand-in for api.krowk.com on :8787
	cargo run --release -p krowk-devregistry --bin devregistry

release-check: ## Validate the release layout, the npm launchers and the installer, offline
	bash -n scripts/dist.sh
	node --check npm/krowk/bin/krowk.js
	node --check npm/mcp/bin/krowk-mcp.js
	# The installer downloads what this file produces, so it belongs to the
	# release pipeline rather than to `check`: it needs python3,
	# which a plain test run has no business requiring.
	scripts/install_test.sh

dist: ## The whole release, locally: every binary, the archives, the npm packages (needs zig + cargo-zigbuild, on macOS)
	scripts/dist.sh all $(VERSION)
	node npm/build.mjs

clean:
	rm -rf bin dist
	cargo clean

# The stand-in registry the golden cases and the integration tests run
# against, at the path they look for it. Built by no release.
bin/devregistry:
	cargo build --release -p krowk-devregistry --bin devregistry
	# rm first: on macOS a rebuilt binary copied over the old one keeps the old
	# inode's code signature, and the kernel kills it on launch.
	mkdir -p bin && rm -f bin/devregistry && cp target/release/devregistry bin/devregistry

# The cases compare the version like any other output, so the build they run is
# stamped with one no release will ever carry.
GOLDEN_VERSION := 0.0.0-golden

golden: bin/devregistry ## Hold the build to tests/golden/cases
	KROWK_VERSION=$(GOLDEN_VERSION) cargo build --release -p krowk --features sessions
	cargo test -p krowk-golden

golden-update: bin/devregistry ## Re-record tests/golden/cases after an intended output change
	KROWK_VERSION=$(GOLDEN_VERSION) cargo build --release -p krowk --features sessions
	GOLDEN_UPDATE=1 cargo test -p krowk-golden
