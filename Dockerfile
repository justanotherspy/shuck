# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# Multi-stage build producing a tiny, static, non-root image.
#
# The base images are Chainguard's (minimal, low/zero-CVE, continuously rebuilt).
# They are pinned by digest (supply-chain integrity) and kept current by the
# `docker` Dependabot ecosystem (.github/dependabot.yml), which bumps the digest
# as :latest moves. Resolve the current digest manually with:
#
#   docker buildx imagetools inspect cgr.dev/chainguard/go:latest \
#     --format '{{.Manifest.Digest}}'

# ---- build stage ------------------------------------------------------------
# --platform=$BUILDPLATFORM keeps the toolchain native; we cross-compile to the
# requested TARGET* below, so no QEMU emulation is needed.
FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/go:latest@sha256:87f98746b75cef20b2d6c568d834a464f40b858ee09f3bdca51654b3f314c641 AS build

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
FROM cgr.dev/chainguard/static:latest@sha256:24dd7ff8788fdfadda39eeeaefefb6d1cec6002a545935a5f7e017484053734f

# OCI metadata: lets GHCR, `docker scout`, etc. link the image to its source.
LABEL org.opencontainers.image.source="https://github.com/justanotherspy/shuck" \
      org.opencontainers.image.description="Background monitor for GitHub PR CI failures, reviews, and action pins" \
      org.opencontainers.image.licenses="MIT"

COPY --from=build /shuck /usr/bin/shuck

# Run as the non-root user (65532) that chainguard/static ships with. Set it
# explicitly so the final image's effective USER is not root.
USER nonroot
ENTRYPOINT ["/usr/bin/shuck"]
