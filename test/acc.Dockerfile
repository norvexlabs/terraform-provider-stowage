# The acceptance tests' runner: tofu, the Docker CLI (the suite drives the host's daemon through
# its socket: zot, kind and the installers run beside this container), and the tools Stowage's
# test harness calls. Every piece pinned by digest or checksum.
FROM docker.io/library/docker:28.5.2-cli@sha256:625d9431a9f54c5a2bc90f24f0e1c3d55b1349fd857dd85035f98c2c9acbdd4d AS docker

FROM docker.io/library/debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
ARG TOFU_VERSION=1.12.6
ARG TOFU_SHA256=50a6106fa4de523d09c87af85f3db1dd47535fc005727fdca6852146476b88ec
RUN apt-get update && apt-get install -y --no-install-recommends bash ca-certificates curl git jq python3 openssl \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL -o /tmp/tofu.tgz "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_amd64.tar.gz" \
    && echo "${TOFU_SHA256}  /tmp/tofu.tgz" | sha256sum -c - \
    && tar -xzf /tmp/tofu.tgz -C /usr/local/bin tofu && rm /tmp/tofu.tgz \
    && tofu version
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker /usr/local/libexec/docker/cli-plugins /usr/local/libexec/docker/cli-plugins
