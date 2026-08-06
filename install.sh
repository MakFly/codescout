#!/bin/sh
# codescout installer — Linux and macOS, amd64 and arm64.
#
#   curl -fsSL https://raw.githubusercontent.com/dev-toolings/codescout/main/install.sh | sh
#
# Environment:
#   SCOUT_INSTALL_DIR   where to put the binary (default: first writable of
#                       ~/.local/bin, /usr/local/bin)
#   SCOUT_VERSION       tag to install (default: latest)
#
# POSIX sh on purpose: this has to run under dash on a bare Debian image.

set -eu

REPO="dev-toolings/codescout"
BIN="scout"

say()  { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die()  { printf 'install: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}

# ---------------------------------------------------------------- platform

os=$(uname -s)
arch=$(uname -m)

case "$os" in
    Linux)  os=linux ;;
    Darwin) os=darwin ;;
    *) die "unsupported OS: $os (codescout ships Linux and macOS builds)" ;;
esac

case "$arch" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) die "unsupported architecture: $arch" ;;
esac

asset="${BIN}-${os}-${arch}"

# ------------------------------------------------------------- downloader

if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL "$1" -o "$2"; }
    fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -qO "$2" "$1"; }
    fetch_stdout() { wget -qO- "$1"; }
else
    die "neither curl nor wget found"
fi

version="${SCOUT_VERSION:-}"
if [ -z "$version" ]; then
    url="https://github.com/${REPO}/releases/latest/download/${asset}"
else
    url="https://github.com/${REPO}/releases/download/${version}/${asset}"
fi

# ------------------------------------------------------------ destination

dest="${SCOUT_INSTALL_DIR:-}"
if [ -z "$dest" ]; then
    for d in "$HOME/.local/bin" /usr/local/bin; do
        if [ -d "$d" ] && [ -w "$d" ]; then dest="$d"; break; fi
    done
fi
if [ -z "$dest" ]; then
    dest="$HOME/.local/bin"
    mkdir -p "$dest"
fi
[ -d "$dest" ] || mkdir -p "$dest"
[ -w "$dest" ] || die "$dest is not writable — set SCOUT_INSTALL_DIR, or run with sudo"

# ------------------------------------------------------------------ fetch

tmp=$(mktemp "${TMPDIR:-/tmp}/scout.XXXXXX") || die "cannot create a temp file"
# Clean up the partial download on any exit path, including Ctrl-C.
trap 'rm -f "$tmp"' EXIT INT TERM

say "codescout: downloading ${asset}"
fetch "$url" "$tmp" || die "download failed: $url"

# A 404 from a redirect can land as an HTML page rather than a binary.
if [ ! -s "$tmp" ]; then
    die "downloaded an empty file from $url"
fi
case "$(head -c 4 "$tmp" | tr -d '\0')" in
    "<htm"|"<!DO") die "the release asset ${asset} does not exist for this platform" ;;
esac

chmod +x "$tmp"
# Install atomically so a concurrent `scout` invocation never sees half a file.
mv "$tmp" "${dest}/${BIN}"
trap - EXIT INT TERM

say "codescout: installed ${dest}/${BIN}"

# --------------------------------------------------------------- verify

if ! "${dest}/${BIN}" --version >/dev/null 2>&1; then
    die "the installed binary does not run on this machine"
fi
say "codescout: $("${dest}/${BIN}" --version)"

case ":${PATH}:" in
    *":${dest}:"*) ;;
    *) warn ""
       warn "NOTE: ${dest} is not on your PATH. Add it:"
       warn "      export PATH=\"${dest}:\$PATH\"" ;;
esac

# ripgrep is a hard runtime dependency, not a nicety — say so now rather than
# letting the first search fail.
if ! command -v rg >/dev/null 2>&1; then
    warn ""
    warn "NOTE: ripgrep (rg) is not installed, and scout cannot work without it."
    case "$os" in
        darwin) warn "      brew install ripgrep" ;;
        linux)  warn "      apt install ripgrep   (or dnf/pacman/apk)" ;;
    esac
fi

say ""
say "Try:  scout search <an identifier> ."
say "Docs: https://github.com/${REPO}"
