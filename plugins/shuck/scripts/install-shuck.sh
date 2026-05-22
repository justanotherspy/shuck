#!/usr/bin/env bash
# Ensure the `shuck` binary is available for the plugin.
#
# Strategy: the plugin ships no binary in git (Go binaries are platform
# specific). Instead, on first use this script downloads the signed release
# archive that matches the plugin's own version from GitHub Releases, verifies
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

# CLI release to fetch. Kept in lockstep with the plugin version so the plugin
# and the shuck binary it installs always share a version (a plugin vX.Y.Z
# expects a shuck CLI release vX.Y.Z). Overridable via SHUCK_CLI_VERSION for
# testing or to pin a different binary release.
plugin_json="$PLUGIN_ROOT/.claude-plugin/plugin.json"
version="${SHUCK_CLI_VERSION:-$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$plugin_json" | head -1)}"
if [ -z "$version" ]; then
  echo "shuck: could not determine version (set SHUCK_CLI_VERSION or check $plugin_json)" >&2
  exit 1
fi

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
base="https://github.com/${REPO}/releases/download/v${version}"

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

sha256_of() { # file -> prints hash
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else return 1; fi
}

tmpd="$(mktemp -d)"
trap 'rm -rf "$tmpd"' EXIT

echo "shuck: downloading $archive ..." >&2
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

echo "shuck v${version} installed into the plugin (${os}/${arch})"
