# syntax=docker/dockerfile:1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# Multi-stage build producing a tiny, static, non-root image.
#
# The base images are Chainguard's (minimal, low/zero-CVE, continuously rebuilt).
# They are pinned by digest (supply-chain integrity) and kept current by
# Renovate (renovate.json), which bumps the digest as :latest moves. Resolve the
# current digest manually with:
#
#   docker buildx imagetools inspect cgr.dev/chainguard/go:latest \
#     --format '{{.Manifest.Digest}}'

# ---- build stage ------------------------------------------------------------
# --platform=$BUILDPLATFORM keeps the toolchain native; we cross-compile to the
# requested TARGET* below, so no QEMU emulation is needed.
FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/go:latest@sha256:0e87094aeae3d022d3926c756d7358dcc67bb1028a890989eb4224e2349c8481 AS build

# Run the build as root so the module cache and output path are writable; this
# stage is discarded and never shipped.
USER root

# Build metadata, injected via --build-arg (mirrors the Makefile). shuck only
# carries a version string (in internal/cli.version); COMMIT/DATE are accepted
# for parity with the container workflow but are not wired into the binary.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# Cross-compilation targets supplied by buildx for each requested platform.
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Download modules in their own layer so source-only changes don't re-fetch.
COPY go.mod go.sum ./
RUN go mod download

# Copy the whole tree: main.go //go:embeds plugins/shuck/skills/shuck/SKILL.md,
# so the build needs plugins/ present (see .dockerignore).
COPY . .

ENV CGO_ENABLED=0
RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w -X github.com/justanotherspy/shuck/internal/cli.version=${VERSION}" \
      -o /shuck .

# ---- runtime stage ----------------------------------------------------------
FROM cgr.dev/chainguard/static:latest@sha256:8658e9eaeb2294fa0e49b0667f7d66b4e21fa7690b8d2a4c30cd1d013ceb9eb7

# OCI metadata: lets GHCR, `docker scout`, etc. link the image to its source.
LABEL org.opencontainers.image.source="https://github.com/justanotherspy/shuck" \
      org.opencontainers.image.description="Background monitor for GitHub PR CI failures, reviews, and action pins" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /shuck /usr/bin/shuck

# Run as the non-root user (65532) that chainguard/static ships with. Set it
# explicitly so the final image's effective USER is not root.
USER nonroot
ENTRYPOINT ["/usr/bin/shuck"]
