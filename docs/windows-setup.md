# VectoraDB on Windows

VectoraDB's engine is Linux-only (ZFS + Docker + Postgres). On Windows it runs inside a dedicated
**WSL2** distro — the direct analog of the Lima VM used on macOS. The native `vdb.exe` launcher
forwards every engine command into that distro, so day to day you just type `vdb …`.

## Prerequisites

- **Windows 10 (21H2+) or Windows 11**
- **WSL2** enabled: in an Administrator PowerShell, `wsl --install` (reboot when prompted)
- **Virtualization** enabled in the BIOS/UEFI and the *Virtual Machine Platform* Windows feature on
  (both are turned on by `wsl --install`)

## Install

```powershell
irm https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.ps1 | iex
vdb setup
```

`install.ps1` places `vdb.exe` in `%LOCALAPPDATA%\Programs\vectoradb` (added to your PATH) and stages
the support assets `vdb setup` needs: the Linux engine binary, a **ZFS-enabled WSL2 kernel**, and an
Ubuntu rootfs.

`vdb setup` then, once:

1. points `%UserProfile%\.wslconfig` at the ZFS-enabled kernel and restarts WSL (`wsl --shutdown`);
2. imports a dedicated `vectoradb` WSL2 distro and enables systemd in it;
3. installs Docker + ZFS, loads the `zfs` module, and installs the engine binary;
4. brings the stack up (`vdb start`).

After that, use VectoraDB exactly as on macOS/Linux — `vdb branch create`, `vdb import`, `vdb status`,
etc. Services are reachable from Windows on `localhost` (gateway `:6432`, control API `:8080`, agent
API `:8088`, MinIO console `:9001`) via WSL2 localhost-forwarding.

## Why a custom kernel?

The stock WSL2 kernel ships **no ZFS module**, and ZFS is what powers VectoraDB's instant
copy-on-write branches. So `vdb setup` installs a ZFS-enabled WSL2 kernel and references it from
`.wslconfig`:

```ini
[wsl2]
kernel=C:\\Users\\<you>\\AppData\\Local\\Programs\\vectoradb\\vectoradb-wsl-kernel
```

> **Note:** `.wslconfig kernel=` is **machine-wide** — it applies to *all* your WSL2 distros. The
> VectoraDB kernel is a **superset** of the stock kernel (same config plus ZFS), so your other
> distros keep working. To revert, remove the `kernel=` line and run `wsl --shutdown`.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `WSL is not installed` | Run `wsl --install` in an **Administrator** PowerShell, reboot, retry. |
| `WSL is present but not healthy` | Enable virtualization in the BIOS and the *Virtual Machine Platform* feature; `wsl --update`. |
| Kernel change not taking effect | `wsl --shutdown`, then rerun the failing command. |
| `zpool: command not found` / module errors | Re-run `vdb setup`; confirm the kernel asset is next to `vdb.exe` and `.wslconfig` points at it. |
| `vdb` not found after install | Open a **new** terminal (PATH updates apply to new sessions). |

## Uninstall

```powershell
wsl --unregister vectoradb                       # remove the distro + its data
Remove-Item -Recurse "$env:LOCALAPPDATA\Programs\vectoradb"
# then remove the kernel= line from %UserProfile%\.wslconfig and: wsl --shutdown
```

## Building the WSL2 kernel (maintainers)

The kernel + rootfs assets are produced on a Linux builder / CI, not the user's machine:

```bash
make wsl-kernel   # deploy/wsl-kernel/build.sh + build-rootfs.sh
```

This builds Microsoft's WSL2 kernel with OpenZFS modules (`vectoradb-wsl-kernel`), bakes the matching
modules into an Ubuntu rootfs (`vectoradb-rootfs.tar`), and both are attached to the GitHub release
alongside `vdb-windows-amd64.exe` and `vdb-linux-amd64`. Pin the kernel and ZFS versions together in
`deploy/wsl-kernel/build.sh` and re-run the end-to-end Windows test after any bump.
