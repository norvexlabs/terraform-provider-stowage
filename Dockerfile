# syntax=docker/dockerfile:1
# Every build, test and check runs here: the host needs Docker and nothing else.
#   make check  → target checks   (gofmt, go vet, staticcheck, govulncheck, the hooks' test, go test -race)
#   make build  → target bin      (the provider binary exported to ./bin, only after green checks)
#   make check  → target docs-check too (docs/ regenerated from OpenTofu's schema of the build: no difference)
ARG GO_VERSION=1.27.4

FROM golang:${GO_VERSION}-bookworm AS base
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download && go mod verify
COPY . .

FROM base AS checks
# -race needs cgo; everything else builds static. -p 4 bounds the test binaries run at once:
# the machine's RAM is shared.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    unformatted="$(gofmt -l .)"; \
    if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi; \
    go vet ./...; \
    go tool staticcheck ./...; \
    go tool govulncheck ./...; \
    bash test/hooks.sh; \
    CGO_ENABLED=1 go test -race -count=1 -p 4 ./...; \
    touch /checks.ok

FROM base AS build
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-s -w -buildid= -X main.version=${VERSION}" -o /out/terraform-provider-stowage .
# No binary leaves this file without green checks.
COPY --from=checks /checks.ok /out/.checks.ok

FROM scratch AS bin
COPY --from=build /out/terraform-provider-stowage /

# docs/ as make docs makes it, from OpenTofu's schema of the build (test/schema.sh, the pinned tofu
# whose archive's checksum is checked) and tfplugindocs, into a copy of the tree: any difference
# from the committed docs/ fails, so docs/ never lags the schema, templates/ or examples/.
FROM build AS docs-check
ARG TOFU_VERSION
ARG TOFU_SHA
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    set -eu; \
    test -n "$TOFU_VERSION" && test -n "$TOFU_SHA"; \
    curl -fsSL "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_amd64.tar.gz" -o /tmp/tofu.tgz; \
    echo "${TOFU_SHA}  /tmp/tofu.tgz" | sha256sum -c --quiet -; \
    tar -xzf /tmp/tofu.tgz -C /usr/local/bin tofu; \
    PROVIDER_BIN=/out/terraform-provider-stowage TOFU=/usr/local/bin/tofu bash test/schema.sh > /tmp/schema.json; \
    cp -r /src /tmp/tree; \
    cd /tmp/tree && go tool tfplugindocs generate --provider-name stowage --providers-schema /tmp/schema.json >/dev/null; \
    if ! diff -ru /src/docs /tmp/tree/docs; then echo "docs/ is not what make docs makes: run make docs and commit it" >&2; exit 1; fi; \
    touch /docs.ok
