# Docker is the only host requirement: every target runs the Go toolchain in a container.
GO_VERSION ?= 1.27.4
VERSION    ?= $(shell sed -n 's/^## \[\([^]]*\)\].*/\1/p' CHANGELOG.md | head -n1)
BUILD      := DOCKER_BUILDKIT=1 docker buildx build --build-arg GO_VERSION=$(GO_VERSION) --build-arg VERSION=$(VERSION)
# Module and build caches persist on the host disk, owned by the caller.
GOCACHE_DIR ?= $(HOME)/.cache/terraform-provider-stowage-go
DOCKER_GO  := docker run --rm --memory 4g -u $(shell id -u):$(shell id -g) -e HOME=/tmp -e GOCACHE=/cache/build \
              -e GOMODCACHE=/cache/mod -e GOFLAGS=-trimpath -v $(GOCACHE_DIR):/cache -v $(CURDIR):/src -w /src
GO         := $(DOCKER_GO) golang:$(GO_VERSION)-bookworm

.PHONY: all check build install-local release-check test hooks-test acc-docker acc-k8s acc-image acc-vm fmt tidy docs shell hooks clean

all: check build

$(GOCACHE_DIR):
	mkdir -p $@

check:            ## GATE: gofmt + vet + staticcheck + govulncheck + the hooks' test + race tests
	$(BUILD) --target checks .

build:            ## ./bin/terraform-provider-stowage (only after green checks)
	$(BUILD) --target bin --output type=local,dest=bin .

# install-local: the built provider in OpenTofu's implied local mirror, so a root naming
# `source = "norvexlabs/stowage"` and this version installs it with `tofu init`, no registry.
# The build is linux_amd64. PLUGIN_DIR picks the mirror (default ~/.terraform.d/plugins).
PLUGIN_DIR ?= $(HOME)/.terraform.d/plugins
install-local: build ## the provider in OpenTofu's implied local mirror (PLUGIN_DIR), for tofu init without the registry
	install -D -m 0755 bin/terraform-provider-stowage $(PLUGIN_DIR)/registry.opentofu.org/norvexlabs/stowage/$(VERSION)/linux_amd64/terraform-provider-stowage_v$(VERSION)
	@echo "installed norvexlabs/stowage $(VERSION) in $(PLUGIN_DIR); after a rebuild, run tofu init -upgrade in each root"

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

# The acceptance tests run in a container of their own (test/acc.Dockerfile), capped in memory,
# driving the host's Docker through its socket: the paths they hand it (the work directory in the
# cache, this tree, Stowage's) are mounted at the same paths. ~/.docker holds the registry logins
# Stowage's ship pulls the base images with, and buildx's state.
STOWAGE_REPO ?= $(HOME)/stowage
ACC_IMAGE    := terraform-provider-stowage-acc
ACC_RUN       = docker run --rm --memory 4g --network host -u $(shell id -u):$(shell id -g) \
                --group-add $(shell stat -c %g /var/run/docker.sock) -v /var/run/docker.sock:/var/run/docker.sock \
                -e HOME=$(HOME) -v $(HOME)/.cache:$(HOME)/.cache -v $(HOME)/.docker:$(HOME)/.docker \
                -v $(CURDIR):$(CURDIR):ro -v $(STOWAGE_REPO):$(STOWAGE_REPO):ro -e STOWAGE_REPO=$(STOWAGE_REPO) \
                -w $(CURDIR) $(ACC_IMAGE)

acc-image:
	DOCKER_BUILDKIT=1 docker buildx build --load -t $(ACC_IMAGE) -f test/acc.Dockerfile test

acc-k8s: build acc-image    ## the Kubernetes target through tofu on a kind cluster, against a real Stowage (STOWAGE_REPO, default ~/stowage), in the acceptance runner; prints PASS
	mkdir -p $(HOME)/.cache $(HOME)/.docker
	$(ACC_RUN) bash test/acc-k8s.sh

hooks-test:       ## the hooks' branches (test/hooks.sh), on this machine; make check runs it too
	bash test/hooks.sh

acc-vm: build     ## the VM target against a real VM on TrueNAS: STOWAGE_VM_ENV=<file> [STOWAGE_REPO=<dir>] make acc-vm
	bash test/acc-vm.sh

fmt: | $(GOCACHE_DIR)              ## rewrite sources with gofmt
	$(GO) gofmt -w .

tidy: | $(GOCACHE_DIR)             ## refresh go.mod / go.sum
	$(GO) go mod tidy

# docs: tfplugindocs reads OpenTofu's schema of the provider built here (test/schema.sh, the
# pinned tofu through dev_overrides), so it never builds the provider or downloads Terraform.
docs: build | $(GOCACHE_DIR)       ## regenerate docs/ from OpenTofu's schema and examples/ (tfplugindocs)
	bash test/schema.sh > $(GOCACHE_DIR)/schema.json
	$(GO) go tool tfplugindocs generate --provider-name stowage --providers-schema /cache/schema.json

shell: | $(GOCACHE_DIR)            ## a shell with the toolchain and the repo mounted
	$(DOCKER_GO) -it golang:$(GO_VERSION)-bookworm bash

hooks:            ## commit as the project identity, with the hooks that hold it
	git config core.hooksPath .githooks
	git config user.name "$$(sed 's/ <.*//' .githooks/identity)"
	git config user.email "$$(sed 's/.*<\(.*\)>/\1/' .githooks/identity)"

clean:
	rm -rf bin dist
