# VectoraDB on Windows

VectoraDB's engine is Linux-only (ZFS + Docker + Postgres). On Windows it runs inside a dedicated
**WSL2** distro — the direct analog of the Lima VM used on macOS. The native `vdb.exe` launcher
forwards every engine command into that distro, so day to day you just type `vdb …`.

## Prerequisites

- **Windows 10 (21H2+) or Windows 11**
- **Virtualization** enabled in the BIOS/UEFI and the *Virtual Machine Platform* Windows feature on
  (both are turned on by `wsl --install`)
- **WSL2 with a Linux distribution actually installed.** In an **Administrator PowerShell**:

  ```powershell
  wsl --install            # enables WSL2 and installs Ubuntu by default; reboot when prompted
  ```

  On some machines `wsl --install` enables the feature but installs **no** distribution. The
  VectoraDB installer needs a working distro to read the WSL kernel version (so it can stage the
  matching ZFS module bundle), so confirm one is registered before installing VectoraDB:

  ```powershell
  wsl --list --verbose     # expect at least one distro listed (state Running or Stopped)
  ```

  If the list is empty — or `wsl -e uname -r` prints *"Windows Subsystem for Linux has no installed
  distributions"* — install one explicitly and reboot:

  ```powershell
  wsl --install -d Ubuntu
  ```

  Then confirm WSL runs and note the kernel (VectoraDB ships the ZFS module for exactly this version):

  ```powershell
  wsl -e uname -r          # e.g. 6.6.87.2-microsoft-standard-WSL2
  ```

## Install

Run these **in PowerShell**, not Command Prompt — `irm` and `iex` are PowerShell commands (`irm` is
the alias for `Invoke-RestMethod`). In cmd.exe you get `irm is not recognized`.

```powershell
irm https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.ps1 | iex
vdb setup
```

If PowerShell blocks the script (`running scripts is disabled on this system`), allow it for this
session first, then re-run the install:

```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
```

On **Windows PowerShell 5.1** (the built-in *Windows PowerShell*, not PowerShell 7), release-asset
downloads sometimes fail with *"The request was aborted: The connection was closed unexpectedly."*
Force TLS 1.2 for the session, then re-run the install in the same window:

```powershell
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
irm https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.ps1 | iex
```

After install, **open a new terminal** before running `vdb` — the installer adds it to your PATH,
and PATH changes only apply to new sessions (`vdb is not recognized` otherwise).

`install.ps1` places `vdb.exe` in `%LOCALAPPDATA%\Programs\vectoradb` (added to your PATH) and stages
what `vdb setup` needs: the Linux engine binary, an Ubuntu rootfs, the Postgres image build context,
and the OpenZFS modules built for the WSL2 kernel **your machine is running**.

`vdb setup` then, once:

1. imports a dedicated `vectoradb` WSL2 distro and enables systemd in it;
2. installs Docker, and installs ZFS into that distro's own module tree;
3. verifies `modprobe zfs` actually works before going further;
4. brings the stack up (`vdb start`).

After that, use VectoraDB exactly as on macOS/Linux — `vdb branch create`, `vdb import`, `vdb status`,
etc. Services are reachable from Windows on `localhost` (gateway `:6432`, control API `:8080`, agent
API `:8088`, MinIO console `:9001`) via WSL2 localhost-forwarding.

## Your other WSL distros are not touched

The stock WSL2 kernel ships **no ZFS module**, and ZFS is what powers VectoraDB's instant
copy-on-write branches. VectoraDB does **not** solve that by replacing your kernel.

WSL mounts its module tree as an overlay — the stock modules are a read-only lower layer, and the
upper layer lives on each distro's own disk. You can see it in any distro:

```
$ grep lib/modules /proc/mounts
none /usr/lib/modules/6.6.87.2-microsoft-standard-WSL2 overlay rw,lowerdir=/modules,
     upperdir=/lib/modules/6.6.87.2-microsoft-standard-WSL2/rw/upper,...
```

So `vdb setup` writes `zfs.ko` into the **`vectoradb` distro's** upper layer and runs `depmod`. Your
`%UserProfile%\.wslconfig` is never modified, no `kernel=` or `kernelModules=` line is added, and
Docker Desktop, Rancher Desktop, and your other distros keep running the stock kernel untouched.

The trade-off is that the modules are tied to one exact kernel version. `vdb setup` reads `uname -r`
and looks for the bundle built for it by name, so a WSL kernel bump produces a clear "no ZFS bundle
for this WSL kernel" message rather than a module that silently refuses to load.

## Troubleshooting

| Symptom | Fix |
| --- | --- |
| `irm is not recognized` / `iex is not recognized` | You're in Command Prompt. Open **PowerShell** and run the install command there. |
| `The term '# ' is not recognized` at line 1 | Harmless — an older cached `install.ps1` had a UTF-8 BOM, so PowerShell ran the first comment as a command. The install still completes; ignore it, or re-run to fetch the fixed copy. |
| `running scripts is disabled on this system` | `Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass`, then re-run the install. |
| `The request was aborted: The connection was closed unexpectedly` | Windows PowerShell 5.1 didn't negotiate TLS 1.2. Run `[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12` in the same window, then re-run the install. |
| `vdb is not recognized` after install | Open a **new** terminal (PATH changes apply to new sessions). To use it in the current window: `$env:Path += ";$env:LOCALAPPDATA\Programs\vectoradb"`. |
| `Staging ZFS…` prints a garbled bundle name / *"…has no installed distributions…"* | WSL has no Linux distro, so the installer can't read the kernel to pick the ZFS bundle. Install one (`wsl --install -d Ubuntu`, reboot), then re-run `install.ps1`. |
| `WSL is not installed` | Run `wsl --install` in an **Administrator** PowerShell, reboot, retry. |
| `WSL is present but not healthy` | Enable virtualization in the BIOS and the *Virtual Machine Platform* feature; `wsl --update`. |
| `no ZFS bundle for this WSL kernel (…)` | Your WSL kernel is newer than this VectoraDB release. Update VectoraDB, or build the bundle yourself (below). Do **not** `wsl --update` — that moves the kernel further ahead. |
| `ZFS is not usable in the "vectoradb" distro` | The staged modules were built for a different kernel. Check `wsl -d vectoradb -- uname -r` against the bundle filename in `%LOCALAPPDATA%\Programs\vectoradb`. |
| `systemd did not finish booting` | `wsl --terminate vectoradb`, then re-run `vdb setup`. |
| `the VectoraDB ZFS pool device is not ready` | Deliberate stop: the pool could not be imported, and VectoraDB will not run the engine in case it recreates the pool over your data. Run `wsl --terminate vectoradb` and retry; if it persists, see `journalctl -u vectoradb-zpool.service` inside the distro. |
| `pool I/O is currently suspended` | The pool lost its backing device. `wsl --terminate vectoradb` and re-run `vdb setup`; the pool is re-imported at boot. |
| `wsl` commands hang and `wsl --shutdown` never returns | A suspended ZFS pool can wedge the WSL VM. In an **Administrator** PowerShell: `Restart-Service WSLService -Force` (a reboot also clears it). |
| `vdb` not found after install | Open a **new** terminal (PATH updates apply to new sessions). |

## Uninstall

```powershell
wsl --unregister vectoradb                       # remove the distro + its data
wsl --shutdown                                   # release its loop device
Remove-Item -Recurse "$env:LOCALAPPDATA\Programs\vectoradb"
Remove-Item -Recurse "$env:LOCALAPPDATA\vectoradb"
```

Nothing else to undo — VectoraDB made no machine-wide WSL changes.

> **The `wsl --shutdown` matters if you plan to reinstall.** Unregistering a
> distro does not release the loop device its ZFS pool was using: that binding
> belongs to the WSL virtual machine, which all distros share, and it survives
> until the VM restarts. A reinstall in the same VM lifetime would otherwise find
> a device pointing at the deleted pool image. `vdb setup` detects this and stops
> with instructions rather than proceeding, but a `wsl --shutdown` avoids it.

## Building the ZFS bundle (maintainers)

The ZFS artifact is produced on a Linux builder / CI (a WSL2 Ubuntu distro works), not on the user's
machine:

```bash
make wsl-zfs
```

This builds the Microsoft WSL2 kernel tree purely as a **compile target** (OpenZFS needs a configured
tree with `Module.symvers`), builds OpenZFS against it, and packages the modules plus matching
userland as `dist/vectoradb-zfs-<kernelrelease>.tar.gz`. The kernel image itself is not shipped.

Pin `KERNEL_TAG` in `deploy/wsl-zfs/build.sh` to the tag matching the kernel WSL ships, and
`EXPECT_RELEASE` to that kernel's `uname -r`; the build **fails loudly** on a mismatch, because
modules built for the wrong kernel are the single most likely way this breaks. Bump `KERNEL_TAG` and
`ZFS_TAG` together and re-run the end-to-end Windows test after any bump.

> **Gotcha:** the build exports an empty `LOCALVERSION`. `scripts/setlocalversion` appends `+` to
> the release whenever that variable is unset and the tree isn't a cleanly-tagged checkout — which
> a `git clone --depth 1` always looks like, because `git describe --exact-match` can't resolve the
> tag in a shallow clone. Without it you get `…-microsoft-standard-WSL2+`, whose vermagic will not
> load on the real `…-microsoft-standard-WSL2` kernel. (An empty `.scmversion` does *not* fix this.)
> The `EXPECT_RELEASE` assertion is what catches it, so do not weaken it into a warning.
