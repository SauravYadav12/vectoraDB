#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Build the prebuilt VectoraDB WSL2 distro image.
#
# Everything that does not depend on the WSL kernel is baked in here: Docker, the
# btrfs tools, the vdb engine binary, the Postgres image build context, and the
# container images the stack runs. `vdb setup` then only has to import this,
# mount its storage, and start. btrfs is in the stock WSL kernel, so unlike ZFS
# there is nothing kernel-specific to build or match.
#
# That removes the slowest and least reliable parts of an install: apt-get on a
# distro mirror, a docker build, and three registry pulls -- each of which is a
# network round trip that can fail halfway and leave a half-built distro. It also
# removes them from every user's machine and does them once, here.
#
# Output: dist/vectoradb-distro.tar.gz
#
# Runs on a Linux builder with Docker (CI: ubuntu-latest). Needs
# dist/vdb-linux-amd64 to exist first --
# `make release` produces it.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$here/../.."
out="$repo/dist"
work="${WORK:-$(mktemp -d)}"
root="$work/root"

BASE_URL="${BASE_URL:-https://cloud-images.ubuntu.com/wsl/releases/noble/current/ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz}"

engine="$out/vdb-linux-amd64"
[ -f "$engine" ] || { echo "error: missing $engine (run 'make release' first)" >&2; exit 1; }

mkdir -p "$out" "$root"

# Two halves of this build touch nothing in common: preparing the rootfs (fetch,
# extract, chroot apt) and preparing the container images (docker build, pulls,
# save). Both are largely network-bound, so running them concurrently overlaps
# the waiting rather than the CPU. They join at the repackage step.
#
# Every phase prints its own elapsed time. CI logs give a total and little else,
# and guessing which step dominates is how the wrong thing gets optimised.
timer(){ printf '    %s: %ss\n' "$1" "$(( $(date +%s) - $2 ))"; }

prepare_rootfs() {
	local t=$(date +%s)
	echo "==> base Ubuntu rootfs"
	curl -fsSL "$BASE_URL" -o "$work/base.tar.gz"
	sudo tar -C "$root" -xpf "$work/base.tar.gz"

	echo "==> engine binary and image build context"
	sudo install -m 0755 "$engine" "$root/usr/local/bin/vdb"
	sudo mkdir -p "$root/usr/local/share/vectoradb/docker/postgres"
	sudo cp "$repo/docker/postgres/Dockerfile" \
		"$repo/docker/postgres/restore-entrypoint.sh" \
		"$root/usr/local/share/vectoradb/docker/postgres/"
	sudo chmod +x "$root/usr/local/share/vectoradb/docker/postgres/restore-entrypoint.sh"

	echo "==> distro configuration"
	# Matches what importDistro would otherwise write at setup time. systemd for
	# the units; root as default user so the engine's sudo never prompts;
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

	echo "==> Docker and btrfs tools, inside the image"
	sudo cp /etc/resolv.conf "$root/etc/resolv.conf"
	sudo chroot "$root" /bin/sh -c '
		set -e
		export DEBIAN_FRONTEND=noninteractive
		apt-get update -y
		apt-get install -y --no-install-recommends docker.io btrfs-progs
		apt-get clean
		rm -rf /var/lib/apt/lists/*
		systemctl enable docker
		ldconfig
	'
	timer "rootfs" "$t"
}

prepare_images() {
	local t=$(date +%s)
	echo "==> preload container images"
	# Built and pulled with the *builder's* Docker, then saved as a tarball that
	# `vdb setup` loads once.
	#
	# Deliberately not a dockerd inside the chroot: that needs /proc, /sys and
	# /dev bind-mounted, and a second daemon then shares the builder's kernel and
	# cgroups with whatever Docker is already running there. save/load keeps the
	# two completely separate, works the same in CI, and still removes every
	# registry round trip from the user's machine -- which is the point.
	#
	# Saved to $work, not into $root: this runs before the rootfs necessarily
	# exists. The repackage step moves it in.
	docker build -t vectoradb/postgres-walg:16 "$repo/docker/postgres"
	docker pull -q minio/minio:latest
	docker pull -q minio/mc:latest
	docker save -o "$work/vectoradb-images.tar" \
		vectoradb/postgres-walg:16 minio/minio:latest minio/mc:latest
	timer "images" "$t"
}

started=$(date +%s)
prepare_rootfs > "$work/rootfs.log" 2>&1 & rootfs_pid=$!
prepare_images > "$work/images.log" 2>&1 & images_pid=$!

rootfs_rc=0; images_rc=0
wait "$rootfs_pid" || rootfs_rc=$?
wait "$images_pid" || images_rc=$?
cat "$work/rootfs.log" "$work/images.log"
[ "$rootfs_rc" -eq 0 ] || { echo "error: rootfs preparation failed" >&2; exit "$rootfs_rc"; }
[ "$images_rc" -eq 0 ] || { echo "error: image preparation failed" >&2; exit "$images_rc"; }
timer "both phases (wall clock)" "$started"

images_dir="$root/usr/local/share/vectoradb/images"
sudo mkdir -p "$images_dir"
sudo mv "$work/vectoradb-images.tar" "$images_dir/vectoradb-images.tar"
sudo chmod 0644 "$images_dir/vectoradb-images.tar"

echo "==> repackage"
# pigz, not gzip: this compresses ~1.7 GB and gzip is single-threaded, which made
# it one of the slowest steps for no reason. Output is ordinary gzip, byte-for-
# byte loadable by `wsl --import`, and measured at the same size. Falls back to
# gzip where pigz is unavailable.
t=$(date +%s)
sudo rm -f "$root/etc/resolv.conf"
if command -v pigz >/dev/null 2>&1; then
	sudo tar -C "$root" -cpf - . | pigz -6 -p "$(nproc)" > "$out/vectoradb-distro.tar.gz"
else
	echo "    note: pigz not found, falling back to single-threaded gzip"
	sudo tar -C "$root" -czpf "$out/vectoradb-distro.tar.gz" .
fi
sudo chown "$(id -u):$(id -g)" "$out/vectoradb-distro.tar.gz"
timer "repackage" "$t"

echo
ls -la "$out/vectoradb-distro.tar.gz" | awk '{printf "distro image: %s (%.0f MB)\n", $NF, $5/1048576}'
timer "total" "$started"
echo "vdb setup imports this, mounts its btrfs storage, and starts."
