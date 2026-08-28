#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Produce the Ubuntu WSL rootfs VectoraDB imports on Windows, pre-baked with the
# ZFS kernel modules built by build.sh so `modprobe zfs` works against our custom
# WSL2 kernel. Output: dist/vectoradb-rootfs.tar
#
# Runs on a Linux builder/CI. Requires dist/vectoradb-modules.tar (from build.sh).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
out="$here/../../dist"
# Ubuntu publishes WSL rootfs tarballs; pin one:
BASE_URL="${BASE_URL:-https://cloud-images.ubuntu.com/wsl/noble/current/ubuntu-noble-wsl-amd64-ubuntu.rootfs.tar.gz}"
work="${WORK:-$(mktemp -d)}"
root="$work/root"
mkdir -p "$root"

echo "==> base rootfs"
curl -fsSL "$BASE_URL" -o "$work/base.tar.gz"
sudo tar -C "$root" -xpf "$work/base.tar.gz"

echo "==> inject ZFS kernel modules"
sudo tar -C "$root" -xf "$out/vectoradb-modules.tar"   # adds /lib/modules/<rel>/…

echo "==> preinstall userland (zfs tools + docker) so first boot is fast"
sudo cp /etc/resolv.conf "$root/etc/resolv.conf" || true
sudo chroot "$root" /bin/sh -c \
  "apt-get update -y && DEBIAN_FRONTEND=noninteractive apt-get install -y zfsutils-linux docker.io && apt-get clean" || \
  echo "note: chroot preinstall skipped (needs binfmt/arch match); vdb setup installs these at first run"

echo "==> repackage"
sudo tar -C "$root" -cpf "$out/vectoradb-rootfs.tar" .
echo "rootfs: $out/vectoradb-rootfs.tar"
