#!/bin/sh
# xlfp installer (Linux/macOS): downloads the matching GitHub Release
# archive, verifies its SHA-256 checksum, and installs the binary.
#
#   curl -fsSL https://raw.githubusercontent.com/tyk-swe/xlfp/main/install.sh | sh
#
# Environment knobs:
#   XLFP_VERSION          release tag to install (default: latest)
#   XLFP_INSTALL_DIR      destination directory (default: ~/.local/bin)
#   XLFP_RELEASE_BASE_URL download base override (testing/mirrors)
#
# While the repository is private, downloads fall back to the GitHub
# CLI (`gh release download`) when plain HTTPS gets a 404, so an
# authenticated `gh` (or GH_TOKEN) works; anonymous download works
# once the repository is public.
set -eu

REPO="tyk-swe/xlfp"
VERSION="${XLFP_VERSION:-}"
INSTALL_DIR="${XLFP_INSTALL_DIR:-$HOME/.local/bin}"

say() { printf '%s\n' "$*"; }
die() { printf 'xlfp install: %s\n' "$*" >&2; exit 1; }

os="$(uname -s)"
case "$os" in
  Linux) os="linux" ;;
  Darwin) os="darwin" ;;
  MINGW*|MSYS*|CYGWIN*) die "Windows shell detected; use install.ps1 from PowerShell instead" ;;
  *) die "unsupported operating system: $os (see https://github.com/$REPO/releases)" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) die "unsupported architecture: $arch" ;;
esac

ASSET="xlfp_${os}_${arch}.tar.gz"
TAG=""
if [ -n "$VERSION" ]; then
  case "$VERSION" in v*) TAG="$VERSION" ;; *) TAG="v$VERSION" ;; esac
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

download_with_curl() {
  if [ -n "${XLFP_RELEASE_BASE_URL:-}" ]; then
    base="$XLFP_RELEASE_BASE_URL"
  elif [ -n "$TAG" ]; then
    base="https://github.com/$REPO/releases/download/$TAG"
  else
    base="https://github.com/$REPO/releases/latest/download"
  fi
  curl -fsSL -o "$TMP/$ASSET" "$base/$ASSET" &&
    curl -fsSL -o "$TMP/checksums.txt" "$base/checksums.txt"
}

download_with_gh() {
  command -v gh >/dev/null 2>&1 || return 1
  if [ -n "$TAG" ]; then
    gh release download "$TAG" --repo "$REPO" --pattern "$ASSET" --pattern "checksums.txt" --dir "$TMP" --clobber
  else
    gh release download --repo "$REPO" --pattern "$ASSET" --pattern "checksums.txt" --dir "$TMP" --clobber
  fi
}

say "Downloading xlfp for ${os}/${arch}${TAG:+ ($TAG)}..."
if ! download_with_curl; then
  if ! download_with_gh; then
    die "download failed. If the repository is still private, install the GitHub CLI, run \`gh auth login\`, and retry — or download a release archive manually from https://github.com/$REPO/releases"
  fi
fi
[ -s "$TMP/$ASSET" ] && [ -s "$TMP/checksums.txt" ] || die "download incomplete"

# Verify the SHA-256 checksum before touching anything executable.
expected="$(awk -v a="$ASSET" '$2 == a {print $1}' "$TMP/checksums.txt")"
[ -n "$expected" ] || die "checksums.txt has no entry for $ASSET"
if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$TMP/$ASSET" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
  actual="$(shasum -a 256 "$TMP/$ASSET" | awk '{print $1}')"
else
  die "need sha256sum or shasum to verify the download"
fi
[ "$actual" = "$expected" ] || die "checksum mismatch for $ASSET (expected $expected, got $actual); refusing to install"
say "Checksum verified."

tar -xzf "$TMP/$ASSET" -C "$TMP"
[ -f "$TMP/xlfp" ] || die "archive did not contain the xlfp binary"

mkdir -p "$INSTALL_DIR"
install -m 0755 "$TMP/xlfp" "$INSTALL_DIR/xlfp"
say "Installed: $INSTALL_DIR/xlfp"
"$INSTALL_DIR/xlfp" version || true

case ":$PATH:" in
  *":$INSTALL_DIR:"*) ;;
  *)
    say ""
    say "Note: $INSTALL_DIR is not on your PATH yet. Add it with:"
    say "  export PATH=\"$INSTALL_DIR:\$PATH\""
    say "(and add that line to your shell's startup file to keep it)."
    ;;
esac

say ""
say "Next steps:"
say "  export XLFP_API_KEY='the key for the endpoint you want to check'"
say "  xlfp            # guided mode, or: xlfp explain"
