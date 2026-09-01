#!/bin/sh
# KeepState CLI installer — https://keepstate.ai/install
# Downloads the ks binary for this OS/arch from the public release, VERIFIES
# its SHA256 against the release's SHA256SUMS, and installs it. A checksum
# mismatch aborts with nothing installed. Uninstall is one line (ks uninstall).
set -eu

REPO="${KS_INSTALL_REPO:-esrygrtc/cli}"
BASE="${KS_INSTALL_BASE:-https://github.com/$REPO/releases/latest/download}"

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

# install location: /usr/local/bin when writable, else ~/.local/bin
DEST="/usr/local/bin"
if [ ! -w "$DEST" ]; then
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
