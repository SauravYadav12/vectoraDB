#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Build a ZFS-enabled WSL2 kernel + matching OpenZFS modules for VectoraDB on
# Windows. The stock WSL2 kernel has no ZFS module, which the engine needs for
# copy-on-write branching. This produces two release assets:
#
#   dist/vectoradb-wsl-kernel     the kernel image (bzImage) — referenced from
#                                 %UserProfile%\.wslconfig  [wsl2] kernel=
#   dist/vectoradb-modules.tar    /lib/modules/<rel>/… incl. zfs.ko, spl.ko —
#                                 baked into the Ubuntu rootfs so `modprobe zfs`
#                                 works against THIS kernel.
#
# Runs on a Linux builder (or CI) with kernel build deps. It is NOT run on the
# user's machine — the artifacts ship in the release; install.ps1 downloads them.
#
# Pinned versions (bump together, then re-test [W-E2E]):
KERNEL_TAG="${KERNEL_TAG:-linux-msft-wsl-6.6.36.6}"   # microsoft/WSL2-Linux-Kernel tag
ZFS_TAG="${ZFS_TAG:-zfs-2.2.4}"                        # openzfs/zfs tag
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
out="$here/../../dist"
work="${WORK:-$(mktemp -d)}"
mkdir -p "$out"

echo "==> deps (Debian/Ubuntu builder)"
sudo apt-get update -y
sudo apt-get install -y build-essential flex bison libssl-dev libelf-dev bc \
  python3 cpio pahole dwarves git autoconf automake libtool uuid-dev \
  zlib1g-dev libblkid-dev tar

echo "==> fetch kernel ($KERNEL_TAG) + zfs ($ZFS_TAG)"
git clone --depth 1 -b "$KERNEL_TAG" https://github.com/microsoft/WSL2-Linux-Kernel "$work/kernel"
git clone --depth 1 -b "$ZFS_TAG"    https://github.com/openzfs/zfs             "$work/zfs"

echo "==> configure + build the kernel with the Microsoft WSL config"
cd "$work/kernel"
cp Microsoft/config-wsl .config
make olddefconfig
make -j"$(nproc)"
KREL="$(make -s kernelrelease)"

echo "==> build OpenZFS modules against the kernel ($KREL)"
cd "$work/zfs"
./autogen.sh
./configure --with-linux="$work/kernel" --with-linux-obj="$work/kernel"
make -j"$(nproc)"
make INSTALL_MOD_PATH="$work/modroot" install   # populates /lib/modules/$KREL

echo "==> package artifacts"
cp "$work/kernel/arch/x86/boot/bzImage" "$out/vectoradb-wsl-kernel"
tar -C "$work/modroot" -cf "$out/vectoradb-modules.tar" "lib/modules/$KREL"
echo "kernel:  $out/vectoradb-wsl-kernel  ($KREL)"
echo "modules: $out/vectoradb-modules.tar"
echo
echo "Next: bake vectoradb-modules.tar into the Ubuntu WSL rootfs (deploy/wsl-kernel/build-rootfs.sh)"
echo "so the shipped distro already has zfs.ko for this kernel; then attach both to the release."
