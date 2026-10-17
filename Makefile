BINARY   := linkpulse
PKG      := github.com/marcus-girolneto/linkpulse
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
ARGS     ?= --help

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: build
build: ## Build the binary into ./bin
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/$(BINARY)

.PHONY: run
run: ## Run the CLI (pass ARGS="https://example.com")
	go run ./cmd/$(BINARY) $(ARGS)

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests with coverage and print a per-function summary
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 1

.PHONY: bench
bench: ## Run benchmarks
	go test -run='^$$' -bench=. -benchmem ./...

.PHONY: fuzz
fuzz: ## Fuzz the URL normalizer and robots.txt parser for 30s each
	go test -run='^$$' -fuzz=FuzzNormalize -fuzztime=30s ./internal/urlnorm
	go test -run='^$$' -fuzz=FuzzParse -fuzztime=30s ./internal/robots

.PHONY: golden
golden: ## Regenerate golden files
	go test ./internal/extract ./internal/report -update

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run ./...

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: docker
docker: ## Build the container image
	docker build -t $(BINARY):$(VERSION) --build-arg VERSION=$(VERSION) .

.PHONY: snapshot
snapshot: ## Build a local GoReleaser snapshot into ./dist
	goreleaser build --snapshot --clean

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist coverage.out
