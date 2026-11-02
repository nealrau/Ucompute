#!/bin/sh
# Ucompute installer for macOS and Linux (ChromeOS Linux included).
#
#   curl -fsSL https://raw.githubusercontent.com/nealrau/Ucompute/main/install.sh | sh
#
# Installs a single binary. Set UCOMPUTE_INSTALL_DIR to choose where.
set -eu

REPO="nealrau/Ucompute"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) echo "ucompute: unsupported OS '$os'. On Windows, use install.ps1." >&2; exit 1 ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  armv7*|armv6*|armhf) arch=arm ;;
  *) echo "ucompute: unsupported CPU '$arch'" >&2; exit 1 ;;
esac
asset="ucompute_${os}_${arch}"

# Prefer a writable folder that is already on PATH.
dir="${UCOMPUTE_INSTALL_DIR:-}"
if [ -z "$dir" ]; then
  dir="$HOME/.local/bin"
  for d in "$HOME/.local/bin" "$HOME/bin" /opt/homebrew/bin /usr/local/bin; do
    case ":$PATH:" in
      *":$d:"*) if [ -d "$d" ] && [ -w "$d" ]; then dir="$d"; break; fi ;;
    esac
  done
fi
mkdir -p "$dir"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
base="https://github.com/$REPO/releases/latest/download"

if curl -fsSL "$base/$asset" -o "$tmp/ucompute"; then
  if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"; then
    expected=$(grep " $asset\$" "$tmp/checksums.txt" | awk '{print $1}')
    if command -v sha256sum >/dev/null 2>&1; then
      actual=$(sha256sum "$tmp/ucompute" | awk '{print $1}')
    else
      actual=$(shasum -a 256 "$tmp/ucompute" | awk '{print $1}')
    fi
    if [ -n "$expected" ] && [ "$expected" != "$actual" ]; then
      echo "ucompute: checksum mismatch for $asset; not installing" >&2
      exit 1
    fi
  fi
elif command -v go >/dev/null 2>&1; then
  echo "ucompute: no prebuilt binary for $os/$arch; building from source with Go..."
  GOBIN="$tmp" go install "github.com/$REPO/cmd/ucompute@latest"
else
  echo "ucompute: could not download $asset from $base" >&2
  exit 1
fi

chmod +x "$tmp/ucompute"
mv -f "$tmp/ucompute" "$dir/ucompute"
echo "ucompute $("$dir/ucompute" version) installed to $dir/ucompute"

# Restart the background worker, if this machine has one, so it runs the new version.
"$dir/ucompute" service status 2>/dev/null | grep -q '^installed' && "$dir/ucompute" service restart >/dev/null 2>&1 && echo "background worker restarted"

case ":$PATH:" in
  *":$dir:"*) ;;
  *)
    echo
    echo "NOTE: $dir is not on your PATH. Add this line to your shell profile (~/.zshrc or ~/.bashrc):"
    echo "  export PATH=\"$dir:\$PATH\""
    ;;
esac
echo
echo "Next: ucompute setup            (on the machine you work on)"
echo "      ucompute setup --worker   (on the machine that should run the jobs)"
