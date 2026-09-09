ARG BASE_IMAGE=ghcr.io/xeys/compose-unpacker:compose-v5-migration

FROM golang:1.26-alpine AS docker-vals-builder
WORKDIR /src
COPY docker-vals/ .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -trimpath -o /docker-vals ./cmd/docker-vals

FROM alpine:3.20 AS tools
ARG VALS_VERSION=0.46.0
ARG SOPS_VERSION=3.13.3
RUN apk add --no-cache curl
RUN curl -fsSL -o /tmp/vals.tar.gz \
      "https://github.com/helmfile/vals/releases/download/v${VALS_VERSION}/vals_${VALS_VERSION}_linux_amd64.tar.gz" \
    && tar -xzf /tmp/vals.tar.gz -C /tmp \
    && mv /tmp/vals /vals
RUN curl -fsSL -o /sops \
      "https://github.com/getsops/sops/releases/download/v${SOPS_VERSION}/sops-v${SOPS_VERSION}.linux.amd64"

# portainer/base has no shell, so everything below must be COPY --chmod, never RUN.
FROM ${BASE_IMAGE}
COPY --from=docker-vals-builder --chmod=755 /docker-vals /usr/local/lib/docker/cli-plugins/docker-vals
COPY --from=tools --chmod=755 /vals /usr/local/bin/vals
COPY --from=tools --chmod=755 /sops /usr/local/bin/sops
