#!/bin/sh
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# VectoraDB installer. Installs the `vdb` command, then you run one more command:
#
#   macOS:  vdb setup      # creates a local Linux VM and brings everything up
#   Linux:  sudo vdb start # provisions ZFS/Docker/image and brings everything up
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.sh | sh
#
# Env overrides:
#   VDB_VERSION   release tag to install         (default: latest)
#   VDB_REPO      GitHub owner/repo              (default: SauravYadav12/vectoraDB)
#   VDB_DIST      install from a local dir of prebuilt binaries instead of downloading
#   VDB_PREFIX    install prefix                 (default: /usr/local)
set -eu

REPO="${VDB_REPO:-SauravYadav12/vectoraDB}"
VERSION="${VDB_VERSION:-latest}"
PREFIX="${VDB_PREFIX:-/usr/local}"
BINDIR="$PREFIX/bin"
SHAREDIR="$PREFIX/share/vectoradb"

say()  { printf '\033[36m==>\033[0m %s\n' "$*"; }
err()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

# sudo helper: use sudo only if we can't create/write the install dir ourselves.
SUDO=""
if [ "$(id -u)" -ne 0 ] && ! ( install -d "$BINDIR" >/dev/null 2>&1 && [ -w "$BINDIR" ] ); then
	command -v sudo >/dev/null 2>&1 && SUDO="sudo" || err "need root or sudo to install into $BINDIR"
fi

# Detect OS + arch and map to our release-asset naming.
os="$(uname -s)"
case "$os" in
	Darwin) os="darwin" ;;
	Linux)  os="linux" ;;
	*)      err "unsupported OS: $os (macOS and Linux are supported)" ;;
esac
arch="$(uname -m)"
case "$arch" in
	arm64|aarch64) arch="arm64" ;;
	x86_64|amd64)  arch="amd64" ;;
	*)             err "unsupported architecture: $arch" ;;
esac

asset="vdb-$os-$arch"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fetch() { # fetch <url> <dest>
	if command -v curl >/dev/null 2>&1; then
		curl -fSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO "$2" "$1"
	else
		err "need curl or wget"
	fi
}

# Resolve the download URL for a release asset.
asset_url() { # asset_url <asset-name>
	if [ "$VERSION" = "latest" ]; then
		echo "https://github.com/$REPO/releases/latest/download/$1"
	else
		echo "https://github.com/$REPO/releases/download/$VERSION/$1"
	fi
}

# Get the host binary (from a local dist dir, or a GitHub release).
if [ -n "${VDB_DIST:-}" ]; then
	say "Installing from local dir $VDB_DIST"
	[ -f "$VDB_DIST/$asset" ] || err "missing $VDB_DIST/$asset"
	cp "$VDB_DIST/$asset" "$tmp/vdb"
else
	say "Downloading $asset ($VERSION)…"
	fetch "$(asset_url "$asset")" "$tmp/vdb" || err "download failed — check the release exists for $os/$arch"
fi
chmod +x "$tmp/vdb"

say "Installing vdb to $BINDIR"
$SUDO install -d "$BINDIR"
$SUDO install -m 0755 "$tmp/vdb" "$BINDIR/vdb"

# On macOS the engine runs in a Linux VM; stash the matching Linux binary so
# `vdb setup` can install it into the VM without another download.
if [ "$os" = "darwin" ]; then
	linux_asset="vdb-linux-$arch"
	say "Fetching the Linux engine binary ($linux_asset) for the VM…"
	if [ -n "${VDB_DIST:-}" ] && [ -f "$VDB_DIST/$linux_asset" ]; then
		cp "$VDB_DIST/$linux_asset" "$tmp/$linux_asset"
	else
		fetch "$(asset_url "$linux_asset")" "$tmp/$linux_asset" || true
	fi
	if [ -f "$tmp/$linux_asset" ]; then
		$SUDO install -d "$SHAREDIR"
		$SUDO install -m 0755 "$tmp/$linux_asset" "$SHAREDIR/$linux_asset"
	fi
fi

echo
say "Installed vdb $("$BINDIR/vdb" version 2>/dev/null | awk '{print $2}')"
if [ "$os" = "darwin" ]; then
	cat <<EOF

Next step (one time):
  vdb setup      # creates a local Linux VM, provisions it, and starts VectoraDB

Then day-to-day:
  vdb status · vdb branch create qa · vdb stop
EOF
	command -v limactl >/dev/null 2>&1 || cat <<EOF

Note: macOS needs Lima (the VM runner). Install it first:
  brew install lima
EOF
else
	cat <<EOF

Next step:
  sudo vdb start   # provisions ZFS + Docker + image, then brings everything up

Then day-to-day:
  vdb status · vdb branch create qa · vdb stop
EOF
fi
