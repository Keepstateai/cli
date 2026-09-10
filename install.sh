#!/bin/sh
# KeepState CLI installer — https://keepstate.ai/install
# Downloads the ks binary for this OS/arch from the public release, VERIFIES
# its SHA256 against the release's SHA256SUMS, and installs it. A checksum
# mismatch aborts with nothing installed. Uninstall is one line (ks uninstall).
set -eu

REPO="${KS_INSTALL_REPO:-keepstateai/cli}"

# KS-063: install a NAMED release, not "latest".
#
# This used to fetch releases/latest/download while the install page named a
# specific version. They agreed by coincidence, not by construction: between a
# tag push and the page rebuild they can differ, and "latest" is not a version
# anyone can check against a checksum they were shown.
#
# The version is resolved from keepstate.ai/cli-version.txt, which the website
# generates from the SAME release truth its install page renders. So the
# version you are told and the version you get are one value from one source.
# The file is a bare version string; anything unexpected is refused rather
# than pasted into a URL. If it cannot be reached, we fall back to latest and
# SAY SO, because a silent fallback is how the original problem started.
PIN_URL="${KS_INSTALL_PIN_URL:-https://keepstate.ai/cli-version.txt}"
if [ -n "${KS_INSTALL_BASE:-}" ]; then
  BASE="$KS_INSTALL_BASE"
  KS_VERSION="${KS_VERSION:-(explicit base)}"
else
  KS_VERSION="${KS_VERSION:-}"
  if [ -z "$KS_VERSION" ]; then
    KS_VERSION=$(curl -fsSL -m 8 "$PIN_URL" 2>/dev/null | tr -d ' \t\r\n' || true)
  fi
  case "$KS_VERSION" in
    v[0-9]*.[0-9]*.[0-9]*)
      BASE="https://github.com/$REPO/releases/download/$KS_VERSION" ;;
    *)
      echo "note: could not resolve a pinned version from $PIN_URL; using the latest release." >&2
      KS_VERSION="latest"
      BASE="https://github.com/$REPO/releases/latest/download" ;;
  esac
fi
echo "Installing ks $KS_VERSION"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac
case "$OS" in
  darwin|linux) : ;;
  *) echo "unsupported OS: $OS (macOS and Linux today; Windows is on the roadmap)" >&2; exit 1 ;;
esac
ASSET="ks-$OS-$ARCH"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

echo "Downloading $ASSET ..."
curl -fsSL -o "$TMP/$ASSET" "$BASE/$ASSET"
curl -fsSL -o "$TMP/SHA256SUMS" "$BASE/SHA256SUMS"

echo "Verifying checksum ..."
EXPECTED=$(awk -v a="$ASSET" '$2 == a || $2 == "*"a {print $1}' "$TMP/SHA256SUMS")
if [ -z "$EXPECTED" ]; then
  echo "REFUSED: no SHA256SUMS entry for $ASSET — not installing an unverifiable binary." >&2
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$TMP/$ASSET" | awk '{print $1}')
else
  GOT=$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')
fi
if [ "$GOT" != "$EXPECTED" ]; then
  echo "REFUSED: checksum mismatch for $ASSET" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  got:      $GOT" >&2
  echo "Nothing was installed." >&2
  exit 1
fi
echo "Checksum OK ($GOT)"

# install location: KS_INSTALL_DIR if set, else /usr/local/bin when writable,
# else ~/.local/bin
DEST="${KS_INSTALL_DIR:-/usr/local/bin}"
if [ -n "${KS_INSTALL_DIR:-}" ]; then
  mkdir -p "$DEST"
elif [ ! -w "$DEST" ]; then
  DEST="$HOME/.local/bin"
  mkdir -p "$DEST"
fi
install -m 755 "$TMP/$ASSET" "$DEST/ks"
echo "Installed: $DEST/ks"

case ":$PATH:" in
  *":$DEST:"*) : ;;
  *)
    echo ""
    echo "NOTE: $DEST is not on your PATH. Add it:"
    echo "  export PATH=\"$DEST:\$PATH\""
    ;;
esac

echo ""
echo "Next step:"
echo "  ks login"
