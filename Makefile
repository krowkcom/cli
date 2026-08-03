VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/krowkcom/cli/internal/cli.Version=$(VERSION)

.PHONY: build test lint vet fmt check mock install clean

build: ## Build ./bin/krowk
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/krowk ./cmd/krowk

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
	go run ./cmd/krowk-mock

install:
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/krowk

clean:
	rm -rf bin
