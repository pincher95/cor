BINARY      := cor
PKG         := github.com/pincher95/cor
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE  ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(BUILD_DATE)

GO          ?= go
DIST        := dist
COVERAGE    := coverage.out

.PHONY: all build install run test test-race cover lint vet fmt tidy vendor clean help

all: build

## build: compile the cor binary into ./$(BINARY)
build:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BINARY) .

## install: install the binary into $GOBIN
install:
	$(GO) install -trimpath -ldflags '$(LDFLAGS)' .

## run: build and run the binary; pass args via ARGS, e.g. make run ARGS='volumes --region us-east-1'
run: build
	./$(BINARY) $(ARGS)

## test: run unit tests
test:
	$(GO) test ./...

## test-race: run unit tests with the race detector
test-race:
	$(GO) test -race ./...

## cover: produce a coverage profile at $(COVERAGE)
cover:
	$(GO) test -coverprofile=$(COVERAGE) ./...
	$(GO) tool cover -func=$(COVERAGE) | tail -1

## lint: run golangci-lint
lint:
	golangci-lint run ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## fmt: run gofmt -s -w on the tree
fmt:
	gofmt -s -w .

## tidy: run go mod tidy
tidy:
	$(GO) mod tidy

## vendor: refresh the vendor directory
vendor:
	$(GO) mod vendor

## dist: cross-compile release binaries into ./$(DIST)
dist: clean-dist
	@mkdir -p $(DIST)
	GOOS=linux  GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)-linux-amd64 .
	GOOS=linux  GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)-linux-arm64 .
	GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/$(BINARY)-darwin-arm64 .

clean-dist:
	rm -rf $(DIST)

## clean: remove the binary and coverage artifacts
clean: clean-dist
	rm -f $(BINARY) $(COVERAGE)

## help: list available targets
help:
	@awk 'BEGIN {FS = ":.*?## "} /^## / { sub(/^## /, "", $$0); split($$0, a, ": "); printf "  \033[36m%-12s\033[0m %s\n", a[1], a[2] }' $(MAKEFILE_LIST)
