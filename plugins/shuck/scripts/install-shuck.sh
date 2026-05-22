#!/usr/bin/env bash
# Ensure the `shuck` binary is available for the plugin.
#
# Strategy: the plugin ships no binary in git (Go binaries are platform
# specific). Instead, on first use this script asks GitHub for the latest
# release, downloads the signed archive that matches the host OS/arch, verifies
# it against checksums.txt, and drops the `shuck` binary into the plugin's
# bin/ directory — which Claude Code automatically adds to PATH.
#
# It is idempotent and cheap to re-run:
#   - exits silently if `shuck` is already on PATH (respects a user install), or
#   - exits silently if the binary is already in the plugin's bin/.
# Output contract (so the SessionStart hook can compose one message):
#   - stdout: a single line ONLY when a fresh download succeeds; empty otherwise
#   - stderr: verbose diagnostics
#   - exit:   0 if shuck is available afterwards, non-zero if it could not be
set -euo pipefail

REPO="justanotherspy/shuck"

# Resolve the plugin root: provided by Claude Code, or derived from this script.
PLUGIN_ROOT="${CLAUDE_PLUGIN_ROOT:-}"
if [ -z "$PLUGIN_ROOT" ]; then
  here="$(cd "$(dirname "$0")" && pwd)"
  PLUGIN_ROOT="$(cd "$here/.." && pwd)"
fi

bindir="$PLUGIN_ROOT/bin"

# Already satisfied? Stay quiet.
if [ -x "$bindir/shuck" ] || [ -x "$bindir/shuck.exe" ]; then
  exit 0
fi
if command -v shuck >/dev/null 2>&1; then
  exit 0
fi

fetch() { # url dest
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --connect-timeout 10 --max-time 120 --retry 3 -o "$2" "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -q --timeout=120 -O "$2" "$1"
  else
    echo "shuck: need curl or wget to download the binary" >&2
    return 1
  fi
}

# fetch_stdout url -> prints body (used for the GitHub API, with optional auth).
fetch_stdout() {
  local url="$1" auth=()
  local token="${GITHUB_TOKEN:-${GH_TOKEN:-}}"
  if [ -n "$token" ]; then auth=(-H "Authorization: Bearer $token"); fi
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --connect-timeout 10 --max-time 60 --retry 3 "${auth[@]}" "$url"
  elif command -v wget >/dev/null 2>&1; then
    local hdr=()
    if [ -n "$token" ]; then hdr=(--header="Authorization: Bearer $token"); fi
    wget -q --timeout=60 "${hdr[@]}" -O - "$url"
  else
    echo "shuck: need curl or wget to download the binary" >&2
    return 1
  fi
}

sha256_of() { # file -> prints hash
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else return 1; fi
}

# CLI release to fetch. Defaults to whatever GitHub reports as the latest
# release so the plugin always installs the newest published shuck binary.
# Overridable via SHUCK_CLI_VERSION (e.g. v0.2.0) for testing or to pin one.
tag="${SHUCK_CLI_VERSION:-}"
if [ -n "$tag" ]; then
  case "$tag" in v*) ;; *) tag="v$tag" ;; esac
else
  api="https://api.github.com/repos/${REPO}/releases/latest"
  tag="$(fetch_stdout "$api" 2>/dev/null | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1 || true)"
fi
if [ -z "$tag" ]; then
  echo "shuck: could not resolve the latest release (set SHUCK_CLI_VERSION to pin one)" >&2
  exit 1
fi
version="${tag#v}"

# Map uname -> goreleaser os/arch used in the asset names.
case "$(uname -s)" in
  Linux)                 os=linux ;;
  Darwin)                os=darwin ;;
  MINGW*|MSYS*|CYGWIN*)  os=windows ;;
  *) echo "shuck: unsupported OS $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "shuck: unsupported arch $(uname -m)" >&2; exit 1 ;;
esac

if [ "$os" = "windows" ]; then ext="zip"; binname="shuck.exe"; else ext="tar.gz"; binname="shuck"; fi

# Asset names follow .goreleaser.yaml: shuck_<version>_<os>_<arch>.<ext>
archive="shuck_${version}_${os}_${arch}.${ext}"
base="https://github.com/${REPO}/releases/download/${tag}"

tmpd="$(mktemp -d)"
trap 'rm -rf "$tmpd"' EXIT

echo "shuck: downloading $archive ($tag) ..." >&2
if ! fetch "$base/$archive" "$tmpd/$archive"; then
  echo "shuck: download failed ($base/$archive)" >&2
  exit 1
fi

# Verify against checksums.txt (fail closed on mismatch).
if fetch "$base/checksums.txt" "$tmpd/checksums.txt" 2>/dev/null; then
  expected="$(awk -v f="$archive" '$2==f {print $1}' "$tmpd/checksums.txt")"
  if [ -n "$expected" ]; then
    if got="$(sha256_of "$tmpd/$archive")"; then
      if [ "$got" != "$expected" ]; then
        echo "shuck: checksum mismatch for $archive (expected $expected, got $got)" >&2
        exit 1
      fi
    else
      echo "shuck: no sha256 tool available; skipping checksum verification" >&2
    fi
  else
    echo "shuck: $archive not listed in checksums.txt; skipping verification" >&2
  fi
fi

# Extract just the binary.
case "$ext" in
  tar.gz) tar -xzf "$tmpd/$archive" -C "$tmpd" "$binname" ;;
  zip)
    if ! command -v unzip >/dev/null 2>&1; then
      echo "shuck: need 'unzip' to extract $archive" >&2
      exit 1
    fi
    unzip -oq "$tmpd/$archive" "$binname" -d "$tmpd"
    ;;
esac

if [ ! -f "$tmpd/$binname" ]; then
  echo "shuck: $binname not found inside $archive" >&2
  exit 1
fi

# Install atomically into the plugin's bin/.
mkdir -p "$bindir"
mv "$tmpd/$binname" "$bindir/$binname.partial"
chmod +x "$bindir/$binname.partial"
mv "$bindir/$binname.partial" "$bindir/$binname"

echo "shuck ${tag} installed into the plugin (${os}/${arch})"
