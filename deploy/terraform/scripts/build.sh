#!/usr/bin/env bash
# Build one backend binary as a Lambda bootstrap (linux/arm64, static).
# Usage: build.sh <component> <outdir>   — component ∈ ingest|worker|gateway|portal
set -euo pipefail

component="$1"
outdir="$2"
root="$(cd "$(dirname "$0")/../../.." && pwd)"

mkdir -p "${outdir}/${component}"
cd "${root}"
GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -ldflags '-s -w' -tags lambda.norpc \
  -o "${outdir}/${component}/bootstrap" "./cmd/shuck-${component}"
echo "built ${component} ($(du -h "${outdir}/${component}/bootstrap" | cut -f1))"
