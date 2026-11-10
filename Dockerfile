# syntax=docker/dockerfile:1
# Every build, test and check runs here: the host needs Docker and nothing else.
#   make check  → target checks   (gofmt, go vet, staticcheck, govulncheck, go test -race)
#   make build  → target bin      (the provider binary exported to ./bin, only after green checks)
ARG GO_VERSION=1.27.3

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
