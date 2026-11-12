# The acceptance tests' runner: tofu, the Docker CLI (the suite drives the host's daemon through
# its socket: zot, kind and the installers run beside this container), the system ssh the
# provider reaches a VM with, ip (the harness finds the address TrueNAS reaches this host at), and
# the tools Stowage's test harness and the VM root's scripts call
# (xorriso for the seed ISO, oras and zstd for disks, jq). Every piece pinned by digest or
# checksum; tofu's pin is test/tofu.sh's, passed as build arguments by the Makefile.
FROM docker.io/library/docker:28.5.2-cli@sha256:625d9431a9f54c5a2bc90f24f0e1c3d55b1349fd857dd85035f98c2c9acbdd4d AS docker

FROM docker.io/library/debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
ARG TOFU_VERSION
ARG TOFU_SHA
ARG ORAS_VERSION=1.3.6
ARG ORAS_SHA256=66fb0ad7206f3f6fa77ed5b552049d364ccf4b50b1684554f731f76302b52ca0
RUN test -n "${TOFU_VERSION}" && test -n "${TOFU_SHA}" \
    && apt-get update && apt-get install -y --no-install-recommends bash ca-certificates curl git jq python3 openssl \
       openssh-client iproute2 xorriso zstd \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL -o /tmp/tofu.tgz "https://github.com/opentofu/opentofu/releases/download/v${TOFU_VERSION}/tofu_${TOFU_VERSION}_linux_amd64.tar.gz" \
    && echo "${TOFU_SHA}  /tmp/tofu.tgz" | sha256sum -c - \
    && tar -xzf /tmp/tofu.tgz -C /usr/local/bin tofu && rm /tmp/tofu.tgz \
    && tofu version \
    && curl -fsSL -o /tmp/oras.tgz "https://github.com/oras-project/oras/releases/download/v${ORAS_VERSION}/oras_${ORAS_VERSION}_linux_amd64.tar.gz" \
    && echo "${ORAS_SHA256}  /tmp/oras.tgz" | sha256sum -c - \
    && tar -xzf /tmp/oras.tgz -C /usr/local/bin oras && rm /tmp/oras.tgz \
    && oras version
# The user the runs run as (`docker run -u UID:GID`), its home the run's HOME: OpenSSH refuses a
# uid with no passwd entry ("No user exists for uid"). The Makefile passes the caller's.
ARG UID
ARG GID
ARG HOME_DIR
RUN test -n "${UID}" && test -n "${GID}" && test -n "${HOME_DIR}" \
    && { getent group "${GID}" >/dev/null || groupadd -g "${GID}" acc; } \
    && { getent passwd "${UID}" >/dev/null || useradd -u "${UID}" -g "${GID}" -d "${HOME_DIR}" -M -N -s /bin/bash acc; } \
    && getent passwd "${UID}"
COPY --from=docker /usr/local/bin/docker /usr/local/bin/docker
COPY --from=docker /usr/local/libexec/docker/cli-plugins /usr/local/libexec/docker/cli-plugins
