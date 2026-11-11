# Docker is the only host requirement: every target runs the Go toolchain in a container.
GO_VERSION ?= 1.27.4
VERSION    ?= $(shell sed -n 's/^## \[\([^]]*\)\].*/\1/p' CHANGELOG.md | head -n1)
BUILD      := DOCKER_BUILDKIT=1 docker buildx build --build-arg GO_VERSION=$(GO_VERSION) --build-arg VERSION=$(VERSION)
# Module and build caches persist on the host disk, owned by the caller.
GOCACHE_DIR ?= $(HOME)/.cache/terraform-provider-stowage-go
DOCKER_GO  := docker run --rm --memory 4g -u $(shell id -u):$(shell id -g) -e HOME=/tmp -e GOCACHE=/cache/build \
              -e GOMODCACHE=/cache/mod -e GOFLAGS=-trimpath -v $(GOCACHE_DIR):/cache -v $(CURDIR):/src -w /src
GO         := $(DOCKER_GO) golang:$(GO_VERSION)-bookworm

.PHONY: all check build release-check test hooks-test acc-docker fmt tidy docs shell hooks clean

all: check build

$(GOCACHE_DIR):
	mkdir -p $@

check:            ## GATE: gofmt + vet + staticcheck + govulncheck + the hooks' test + race tests
	$(BUILD) --target checks .

build:            ## ./bin/terraform-provider-stowage (only after green checks)
	$(BUILD) --target bin --output type=local,dest=bin .

# GoReleaser's image, pinned by digest; its go command runs the Go of GO_VERSION (GOTOOLCHAIN).
GORELEASER := docker.io/goreleaser/goreleaser:v2.19.0@sha256:b62e651c2fd9e7b1c94133a4e5d131187649e45aeeca137fbf4940f0f654ad08

release-check: | $(GOCACHE_DIR)    ## .goreleaser.yml checked, a snapshot release made in dist/: zips, manifest, checksums (unsigned, unpublished)
	$(DOCKER_GO) -e GOTOOLCHAIN=go$(GO_VERSION) --entrypoint sh $(GORELEASER) -c \
	  'git config --global --add safe.directory /src && goreleaser check && goreleaser release --snapshot --clean --skip=sign,publish --parallelism 2'

test: | $(GOCACHE_DIR)             ## fast loop: unit tests only, no race detector; never the gate
	$(GO) go test -count=1 -p 4 ./...

# STOWAGE_SRC: a Stowage checkout (>= 0.34.0) with ./bin/stowage built; its examples/minimal is shipped.
acc-docker: build ## acceptance: the Docker target against a real installer, through OpenTofu (test/acc-docker.sh)
	STOWAGE_SRC="$(STOWAGE_SRC)" bash test/acc-docker.sh

hooks-test:       ## the hooks' branches (test/hooks.sh), on this machine; make check runs it too
	bash test/hooks.sh

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
