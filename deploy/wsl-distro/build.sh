#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Build the prebuilt VectoraDB WSL2 distro image.
#
# Everything that does not depend on the WSL kernel is baked in here: Docker, the
# ZFS userland, the vdb engine binary, the Postgres image build context, and the
# container images the stack runs. `vdb setup` then only has to import this,
# drop in the matching ZFS modules (~2 MB), and start.
#
# That removes the slowest and least reliable parts of an install: apt-get on a
# distro mirror, a docker build, and three registry pulls -- each of which is a
# network round trip that can fail halfway and leave a half-built distro. It also
# removes them from every user's machine and does them once, here.
#
# Output: dist/vectoradb-distro.tar.gz
#
# Runs on a Linux builder with Docker (CI: ubuntu-latest). Needs
# dist/vectoradb-zfs-userland.tar.gz and dist/vdb-linux-amd64 to exist first --
# `make wsl-zfs` and `make release` produce them.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$here/../.."
out="$repo/dist"
work="${WORK:-$(mktemp -d)}"
root="$work/root"

BASE_URL="${BASE_URL:-https://cloud-images.ubuntu.com/wsl/releases/noble/current/ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz}"

userland="$out/vectoradb-zfs-userland.tar.gz"
engine="$out/vdb-linux-amd64"
for f in "$userland" "$engine"; do
	[ -f "$f" ] || { echo "error: missing $f (run 'make wsl-zfs' and 'make release' first)" >&2; exit 1; }
done

mkdir -p "$out" "$root"

echo "==> base Ubuntu rootfs"
curl -fsSL "$BASE_URL" -o "$work/base.tar.gz"
sudo tar -C "$root" -xpf "$work/base.tar.gz"

echo "==> ZFS userland (kernel-independent half of the ZFS build)"
# --keep-directory-symlink: the tarball carries ./lib, and /lib is a symlink to
# /usr/lib on a usrmerged Ubuntu. Without it tar replaces that symlink with a
# real directory and splits the distro's libraries in two.
sudo tar -C "$root" --keep-directory-symlink -xzf "$userland"

echo "==> engine binary and image build context"
sudo install -m 0755 "$engine" "$root/usr/local/bin/vdb"
sudo mkdir -p "$root/usr/local/share/vectoradb/docker/postgres"
sudo cp "$repo/docker/postgres/Dockerfile" \
	"$repo/docker/postgres/restore-entrypoint.sh" \
	"$root/usr/local/share/vectoradb/docker/postgres/"
sudo chmod +x "$root/usr/local/share/vectoradb/docker/postgres/restore-entrypoint.sh"

echo "==> distro configuration"
# Matches what importDistro would otherwise write at setup time. systemd for the
# units; root as default user so the engine's sudo never prompts;
# appendWindowsPath=false so a machine with Docker Desktop cannot shadow the
# Linux docker with docker.exe.
sudo tee "$root/etc/wsl.conf" >/dev/null <<'WSLCONF'
[boot]
systemd=true

[user]
default=root

[interop]
appendWindowsPath=false
WSLCONF

echo "==> Docker, inside the image"
sudo cp /etc/resolv.conf "$root/etc/resolv.conf"
sudo chroot "$root" /bin/sh -c '
	set -e
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -y
	apt-get install -y --no-install-recommends docker.io
	apt-get clean
	rm -rf /var/lib/apt/lists/*
	systemctl enable docker
	# Importing is vectoradb-zpool.service’s job alone, scoped to the device it
	# attached. A wide "zpool import -d /dev" can adopt an old pool label from a
	# stale loop binding left by an unregistered distro.
	systemctl mask zfs-import-scan.service zfs-import-cache.service || true
	systemctl enable zfs-mount.service zfs.target || true
	ldconfig
'

echo "==> preload container images"
# Built and pulled with the *builder's* Docker, then saved into the image as a
# tarball that `vdb setup` loads once.
#
# Deliberately not a dockerd inside the chroot: that needs /proc, /sys and /dev
# bind-mounted, and a second daemon then shares the builder's kernel and cgroups
# with whatever Docker is already running there. save/load keeps the two
# completely separate, works the same in CI, and still removes every registry
# round trip from the user's machine -- which is the point.
images_dir="$root/usr/local/share/vectoradb/images"
sudo mkdir -p "$images_dir"

docker build -t vectoradb/postgres-walg:16 "$repo/docker/postgres"
docker pull minio/minio:latest
docker pull minio/mc:latest

sudo docker save -o "$images_dir/vectoradb-images.tar" \
	vectoradb/postgres-walg:16 minio/minio:latest minio/mc:latest
sudo chmod 0644 "$images_dir/vectoradb-images.tar"

echo "==> repackage"
sudo rm -f "$root/etc/resolv.conf"
sudo tar -C "$root" -czpf "$out/vectoradb-distro.tar.gz" .
sudo chown "$(id -u):$(id -g)" "$out/vectoradb-distro.tar.gz"

echo
ls -la "$out/vectoradb-distro.tar.gz" | awk '{printf "distro image: %s (%.0f MB)\n", $NF, $5/1048576}'
echo "vdb setup imports this, adds the per-kernel ZFS modules, and starts."
