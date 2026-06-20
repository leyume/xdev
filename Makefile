# xdev — developer Makefile
#
# Go lives in Homebrew on this machine; make sure it's on PATH for every recipe.
export PATH := /opt/homebrew/bin:$(PATH)

BINARY := xdev
PKG    := ./cmd/xdev

# Version is derived from git (tag-aware), falling back to "dev" outside a repo
# or before the first tag. The release workflow overrides this with the tag.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

# Cross-compile matrix for `build-all` (local testing of the release targets).
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

.PHONY: run build build-linux build-all checksums tidy fmt vet test hooks clean

# Build and run the control plane (default: http://127.0.0.1:7331).
run:
	go run $(PKG)

# Compile a single static binary into ./$(BINARY), stamped with the version.
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)

# Cross-compile for an Ubuntu server (amd64). Pure-Go deps → no CGO needed.
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-linux-amd64 $(PKG)

# Cross-compile every release target into dist/ (the same set the release
# workflow publishes). For local verification — the real release is built by
# GitHub Actions on a tag.
build-all:
	@mkdir -p dist
	@for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "  building dist/$(BINARY)-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 \
			go build -ldflags "$(LDFLAGS)" -o dist/$(BINARY)-$$os-$$arch $(PKG) || exit 1; \
	done

# Generate dist/checksums.txt (sha256 of every built binary).
checksums: build-all
	@cd dist && shasum -a 256 $(BINARY)-* > checksums.txt && echo "  wrote dist/checksums.txt"

tidy:
	go mod tidy

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test ./...

# Enable the in-repo git hooks (the pre-push build/vet/test gate). Run once.
hooks:
	git config core.hooksPath .githooks
	@echo "  git hooks enabled (.githooks) — pre-push runs build/vet/test"

clean:
	rm -rf $(BINARY) dist
