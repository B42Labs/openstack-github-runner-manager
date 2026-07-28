# SPDX-FileCopyrightText: Copyright 2026 B42Labs contributors
# SPDX-License-Identifier: BUSL-1.1

# Makefile for ogrm (openstack-github-runner-manager).
#
# The repository is a single standalone Go module. GOWORK is pinned to off so
# every go invocation resolves against this module's own go.mod even when the
# checkout sits below an unrelated go.work workspace.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

export GOWORK := off

BIN ?= bin/ogrm

# Build metadata linked into the binary via -ldflags -X. Override on the CLI:
# make build VERSION=v0.1.0
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: help build test test-go test-disk-guard vet lint tidy fmt run clean

help: ## Print this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| sort \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n",$$1,$$2}'

build: ## Build the ogrm binary into bin/
	go build -ldflags "$(LDFLAGS)" -o $(BIN) .

test: test-go test-disk-guard ## Run every unit test (Go and the disk guard)

test-go: ## Run the Go unit tests
	go test ./...

test-disk-guard: ## Exercise the disk guard install.sh generates (no root, no Docker)
	./hack/test-disk-guard.sh

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint over the module (requires golangci-lint >= 1.26 toolchain)
	golangci-lint run ./...

tidy: ## Tidy go.mod and go.sum
	go mod tidy

fmt: ## Format the Go sources
	gofmt -w .

run: build ## Build and run, e.g. make run ARGS="create -name acme -count 2"
	$(BIN) $(ARGS)

clean: ## Remove build artifacts
	rm -rf bin
