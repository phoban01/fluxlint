#!/bin/sh
# Installs a released fluxlint binary.
#
#   curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh
#   curl -sSfL https://raw.githubusercontent.com/phoban01/fluxlint/main/install.sh | sh -s -- -b /usr/local/bin v0.1.0
#
# The archive is checked against the release's checksums.txt before anything is
# installed.
set -eu

REPO="phoban01/fluxlint"
BINDIR="${BINDIR:-$HOME/.local/bin}"
VERSION="${VERSION:-}"

usage() {
	cat <<EOF
usage: install.sh [-b bindir] [version]

  -b bindir   where to put the binary (default: \$HOME/.local/bin, or \$BINDIR)
  version     a release tag such as v0.1.0 (default: the latest release)
EOF
}

while getopts "b:h" opt; do
	case "$opt" in
	b) BINDIR="$OPTARG" ;;
	h) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
shift $((OPTIND - 1))
[ $# -gt 0 ] && VERSION="$1"

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

fetch() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -sSfL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		fail "curl or wget is required"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		fail "sha256sum or shasum is required"
	fi
}

need uname
need tar
need mktemp

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
linux | darwin) ;;
*) fail "unsupported OS $os: download a release from https://github.com/$REPO/releases" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "unsupported architecture $arch" ;;
esac

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

if [ -z "$VERSION" ]; then
	# the public API needs no token, and sed saves a dependency on jq
	fetch "https://api.github.com/repos/$REPO/releases/latest" "$tmp/latest.json" ||
		fail "cannot find the latest release; pass a version, e.g. v0.1.0"
	VERSION=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)
	[ -n "$VERSION" ] || fail "cannot read the latest release tag; pass a version, e.g. v0.1.0"
fi

archive="fluxlint_${VERSION#v}_${os}_${arch}.tar.gz"
# FLUXLINT_BASE_URL points at a mirror that holds the same files
base="${FLUXLINT_BASE_URL:-https://github.com/$REPO/releases/download/$VERSION}"

echo "fluxlint $VERSION $os/$arch"
fetch "$base/$archive" "$tmp/$archive" || fail "cannot download $base/$archive"
fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "cannot download $base/checksums.txt"

want=$(awk -v f="$archive" '$2 == f {print $1}' "$tmp/checksums.txt")
[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
got=$(sha256 "$tmp/$archive")
[ "$want" = "$got" ] || fail "checksum mismatch for $archive: want $want, got $got"

tar -xzf "$tmp/$archive" -C "$tmp"
[ -f "$tmp/fluxlint" ] || fail "$archive does not contain fluxlint"
mkdir -p "$BINDIR"
install -m 0755 "$tmp/fluxlint" "$BINDIR/fluxlint"
echo "installed $BINDIR/fluxlint"

case ":$PATH:" in
*":$BINDIR:"*) ;;
*) echo "note: $BINDIR is not on your PATH" ;;
esac
