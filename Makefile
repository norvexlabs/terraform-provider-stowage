# Docker is the only host requirement: every target runs the Go toolchain in a container.
GO_VERSION ?= 1.27.3
VERSION    ?= $(shell sed -n 's/^## \[\([^]]*\)\].*/\1/p' CHANGELOG.md | head -n1)
BUILD      := DOCKER_BUILDKIT=1 docker buildx build --build-arg GO_VERSION=$(GO_VERSION) --build-arg VERSION=$(VERSION)
# Module and build caches persist on the host disk, owned by the caller.
GOCACHE_DIR ?= $(HOME)/.cache/terraform-provider-stowage-go
DOCKER_GO  := docker run --rm --memory 4g -u $(shell id -u):$(shell id -g) -e HOME=/tmp -e GOCACHE=/cache/build \
              -e GOMODCACHE=/cache/mod -e GOFLAGS=-trimpath -v $(GOCACHE_DIR):/cache -v $(CURDIR):/src -w /src
GO         := $(DOCKER_GO) golang:$(GO_VERSION)-bookworm

.PHONY: all check build test fmt tidy docs shell hooks clean

all: check build

$(GOCACHE_DIR):
	mkdir -p $@

check:            ## GATE: gofmt + vet + staticcheck + govulncheck + race tests
	$(BUILD) --target checks .

build:            ## ./bin/terraform-provider-stowage (only after green checks)
	$(BUILD) --target bin --output type=local,dest=bin .

test: | $(GOCACHE_DIR)             ## fast loop: unit tests only, no race detector; never the gate
	$(GO) go test -count=1 -p 4 ./...

fmt: | $(GOCACHE_DIR)              ## rewrite sources with gofmt
	$(GO) gofmt -w .

tidy: | $(GOCACHE_DIR)             ## refresh go.mod / go.sum
	$(GO) go mod tidy

docs: | $(GOCACHE_DIR)             ## regenerate docs/ from the schema and examples/ (tfplugindocs; needs network)
	$(GO) go tool tfplugindocs generate --provider-name stowage

shell: | $(GOCACHE_DIR)            ## a shell with the toolchain and the repo mounted
	$(DOCKER_GO) -it golang:$(GO_VERSION)-bookworm bash

hooks:            ## commit as the project identity, with the hooks that hold it
	git config core.hooksPath .githooks
	git config user.name "$$(sed 's/ <.*//' .githooks/identity)"
	git config user.email "$$(sed 's/.*<\(.*\)>/\1/' .githooks/identity)"

clean:
	rm -rf bin dist
