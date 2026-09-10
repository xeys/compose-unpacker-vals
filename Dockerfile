ARG BASE_IMAGE=ghcr.io/xeys/compose-unpacker:compose-v5-migration

FROM alpine:3.20 AS tools
ARG DOCKER_VALS_VERSION=0.2.0
ARG VALS_VERSION=0.46.0
ARG SOPS_VERSION=3.13.3
RUN apk add --no-cache curl
RUN curl -fsSL -o /docker-vals \
      "https://github.com/estie-inc/docker-vals/releases/download/v${DOCKER_VALS_VERSION}/docker-vals-linux-amd64"
RUN curl -fsSL -o /tmp/vals.tar.gz \
      "https://github.com/helmfile/vals/releases/download/v${VALS_VERSION}/vals_${VALS_VERSION}_linux_amd64.tar.gz" \
    && tar -xzf /tmp/vals.tar.gz -C /tmp \
    && mv /tmp/vals /vals
RUN curl -fsSL -o /sops \
      "https://github.com/getsops/sops/releases/download/v${SOPS_VERSION}/sops-v${SOPS_VERSION}.linux.amd64"

# portainer/base has no shell, so everything below must be COPY --chmod, never RUN.
FROM ${BASE_IMAGE}
COPY --from=tools --chmod=755 /docker-vals /usr/local/lib/docker/cli-plugins/docker-vals
COPY --from=tools --chmod=755 /vals /usr/local/bin/vals
COPY --from=tools --chmod=755 /sops /usr/local/bin/sops
