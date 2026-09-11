ARG BASE_IMAGE=ghcr.io/forgelab-me/compose-unpacker:compose-v5-migration

FROM alpine:3.20 AS tools
ARG DOCKER_VALS_VERSION=0.2.0
ARG VALS_VERSION=0.46.0
ARG SOPS_VERSION=3.13.3
# sha256sum of each binary/archive below, pinned so a tampered release asset
# (compromised maintainer account/CI, compromised mirror) gets caught instead
# of silently baked into the image. sops and vals publish their own
# checksums.txt per release (verified against here); docker-vals doesn't
# publish one, so DOCKER_VALS_SHA256 is self-computed on first pin (TOFU) -
# re-verify it by hand against the release if you ever bump that version.
ARG DOCKER_VALS_SHA256=40525605a97edb463f321cd300f70dc3c8774d7ac2fb2b993b2dd36281566daf
ARG VALS_SHA256=42d2f672dc98b040b8179e87b1c3474418003b95a938ba3bbe13310e5e82847c
ARG SOPS_SHA256=e5bec3346a873ae91d871550f3e698c1aad962aff462a080e40f25fde17fef6b
# wget (BusyBox) ships in the base image already, unlike curl - avoids an
# `apk add` round-trip to the Alpine CDN (dl-cdn.alpinelinux.org), which can be
# flaky/unreachable on some networks (IPv6-only resolution, DNS, etc).
RUN wget -qO /docker-vals \
      "https://github.com/estie-inc/docker-vals/releases/download/v${DOCKER_VALS_VERSION}/docker-vals-linux-amd64" \
    && echo "${DOCKER_VALS_SHA256}  /docker-vals" | sha256sum -c -
RUN wget -qO /tmp/vals.tar.gz \
      "https://github.com/helmfile/vals/releases/download/v${VALS_VERSION}/vals_${VALS_VERSION}_linux_amd64.tar.gz" \
    && echo "${VALS_SHA256}  /tmp/vals.tar.gz" | sha256sum -c - \
    && tar -xzf /tmp/vals.tar.gz -C /tmp \
    && mv /tmp/vals /vals
RUN wget -qO /sops \
      "https://github.com/getsops/sops/releases/download/v${SOPS_VERSION}/sops-v${SOPS_VERSION}.linux.amd64" \
    && echo "${SOPS_SHA256}  /sops" | sha256sum -c -

FROM golang:1.26-alpine AS wrapper-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /entrypoint-wrapper .

# portainer/base has no shell, so everything below must be COPY --chmod, never RUN.
FROM ${BASE_IMAGE}
COPY --from=tools --chmod=755 /docker-vals /usr/local/lib/docker/cli-plugins/docker-vals
COPY --from=tools --chmod=755 /vals /usr/local/bin/vals
COPY --from=tools --chmod=755 /sops /usr/local/bin/sops
COPY --from=wrapper-build --chmod=755 /entrypoint-wrapper /usr/local/bin/entrypoint-wrapper

# Pre-decrypts secrets.enc.yaml (if present) before handing off to the real
# compose-unpacker binary, so a stack's compose file can use native
# `secrets: <name>: file: ...` / `environment: ...` instead of the busybox
# _FILE pattern. SECRET_OUTPUT_MODE picks which of the two it produces:
#   - both (default): decrypted files under <destination>/decrypted-secrets/
#     AND --env flags for the real binary
#   - file: only the decrypted files, nothing added to the environment
#   - env:  only the --env flags, no plaintext ever written to the host
# Portainer controls the container's actual args (`deploy <repo> <ref> ...`),
# so this can't be a CLI flag - set it here, or override with `docker run -e`
# / COMPOSE_UNPACKER_IMAGE_ENV-style Portainer env var injection if available.
ARG SECRET_OUTPUT_MODE=both
ENV SECRET_OUTPUT_MODE=${SECRET_OUTPUT_MODE}

ENTRYPOINT ["/usr/local/bin/entrypoint-wrapper"]
