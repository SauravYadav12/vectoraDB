#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Build OpenZFS kernel modules + matching userland for the *stock* WSL2 kernel.
#
# VectoraDB needs ZFS for copy-on-write branching and the WSL2 kernel ships no
# zfs module. It does not, however, need a custom kernel: WSL mounts its module
# tree as an overlay whose lower layer is the stock module set and whose upper
# layer lives on the distro's own disk (see /proc/mounts inside any distro):
#
#   none /usr/lib/modules/<rel> overlay lowerdir=/modules,upperdir=/lib/modules/<rel>/rw/upper,...
#
# So `vdb setup` just drops zfs.ko/spl.ko into the vectoradb distro's own module
# overlay and runs depmod. Nothing outside that distro is touched — no custom
# kernel, no .wslconfig edit, no effect on Docker Desktop / Rancher Desktop.
#
# A kernel tree is still built here, but only as a *compile target*: OpenZFS
# needs a configured tree with Module.symvers to build against. The kernel image
# itself is not shipped.
#
# Output:
#   dist/vectoradb-zfs-<kernelrelease>.tar.gz   modules under lib/modules/<rel>/extra
#                                               + userland under usr/local
#   dist/vectoradb-zfs.release                  the kernel release string
#
# Runs on a Linux builder or CI (including a WSL2 Ubuntu distro on the target
# machine). It is NOT run on the user's machine — install.ps1 downloads the
# artifact from the release.
#
# KERNEL_TAG MUST match the kernel WSL actually runs (`uname -r` inside WSL).
# A mismatched vermagic makes `modprobe zfs` fail — that is the single most
# likely breakage, so the build asserts it below. Bump both pins together and
# re-run the end-to-end Windows test.
KERNEL_TAG="${KERNEL_TAG:-linux-msft-wsl-6.6.87.2}"      # microsoft/WSL2-Linux-Kernel tag
ZFS_TAG="${ZFS_TAG:-zfs-2.3.9}"                          # openzfs/zfs tag
EXPECT_RELEASE="${EXPECT_RELEASE:-6.6.87.2-microsoft-standard-WSL2}"
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
out="$here/../../dist"
work="${WORK:-$(mktemp -d)}"
stage="$work/stage"
mkdir -p "$out" "$stage"

echo "==> deps (Debian/Ubuntu builder)"
sudo apt-get update -y
sudo apt-get install -y build-essential flex bison libssl-dev libelf-dev bc \
  python3 cpio pahole dwarves git autoconf automake libtool uuid-dev \
  zlib1g-dev libblkid-dev libudev-dev libaio-dev libattr1-dev libcurl4-openssl-dev \
  libtirpc-dev python3-dev python3-setuptools python3-cffi python3-packaging tar

echo "==> fetch kernel ($KERNEL_TAG) + zfs ($ZFS_TAG)"
git clone --depth 1 -b "$KERNEL_TAG" https://github.com/microsoft/WSL2-Linux-Kernel "$work/kernel"
git clone --depth 1 -b "$ZFS_TAG"    https://github.com/openzfs/zfs             "$work/zfs"

echo "==> configure + build the kernel with the Microsoft WSL config"
# Full build, not just modules_prepare: OpenZFS needs Module.symvers, which only
# a complete build produces.
cd "$work/kernel"
# Exporting LOCALVERSION (even empty) stops scripts/setlocalversion appending
# "+" to the release. It appends that whenever the env var is unset and the tree
# isn't a cleanly-tagged checkout — which a `clone --depth 1` always looks like,
# since `git describe --exact-match` cannot resolve the tag in a shallow clone.
# Without this the build yields "…-WSL2+", whose vermagic does not match the
# running "…-WSL2" kernel, and the modules refuse to load.
export LOCALVERSION=
cp Microsoft/config-wsl .config
make olddefconfig
make -j"$(nproc)"
KREL="$(make -s kernelrelease)"

# The vermagic contract. If this trips, the pinned KERNEL_TAG no longer matches
# the kernel WSL ships; find the tag for `uname -r` and bump KERNEL_TAG.
if [ "$KREL" != "$EXPECT_RELEASE" ]; then
	echo "error: kernel release mismatch" >&2
	echo "  built:    $KREL" >&2
	echo "  expected: $EXPECT_RELEASE  (from EXPECT_RELEASE)" >&2
	echo "  modules built here would fail to load on the target WSL kernel." >&2
	exit 1
fi
echo "    kernel release: $KREL"

echo "==> build OpenZFS against the kernel ($KREL)"
cd "$work/zfs"
./autogen.sh
# /usr/local keeps our userland clear of the distro's dpkg-managed /usr, so the
# shipped binaries and libs can never be half-upgraded by apt. systemd units are
# installed too: the pool lives on a file vdev and must be re-imported when the
# distro restarts (WSL stops idle distros), and the engine never imports it
# itself — it would otherwise fall through to `zpool create -f`.
./configure \
	--prefix=/usr/local \
	--with-linux="$work/kernel" \
	--with-linux-obj="$work/kernel" \
	--with-systemdunitdir=/usr/local/lib/systemd/system \
	--with-udevdir=/usr/local/lib/udev
make -j"$(nproc)"
make DESTDIR="$stage" INSTALL_MOD_PATH="$stage" install

echo "==> package"
# Drop libtool archives and build-tree symlinks; keep modules + userland.
find "$stage" -name '*.la' -delete
rm -f "$stage/lib/modules/$KREL/build" "$stage/lib/modules/$KREL/source"
tarball="$out/vectoradb-zfs-$KREL.tar.gz"
tar -C "$stage" -czf "$tarball" .
printf '%s\n' "$KREL" > "$out/vectoradb-zfs.release"

echo
echo "zfs modules + userland: $tarball"
echo "kernel release:         $out/vectoradb-zfs.release ($KREL)"
echo
echo 'Attach both to the GitHub release; install.ps1 stages them and `vdb setup`'
echo "extracts the tarball into the vectoradb distro, then runs depmod + modprobe zfs."
