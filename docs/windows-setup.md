# VectoraDB on Windows

VectoraDB's engine is Linux-only (ZFS + Docker + Postgres). On Windows it runs inside a dedicated
**WSL2** distro — the direct analog of the Lima VM used on macOS. The native `vdb.exe` launcher
forwards every engine command into that distro, so day to day you just type `vdb …`.

## Prerequisites

- **Windows 10 (21H2+) or Windows 11**
- **Virtualization** enabled in the BIOS/UEFI

That's it. You do **not** need to install WSL yourself, and you do **not** need Ubuntu or any other
Linux distribution — VectoraDB installs WSL if it's missing and brings its own dedicated distro.

## Install

One command, **in PowerShell** — not Command Prompt. `irm` and `iex` are PowerShell commands (`irm`
is the alias for `Invoke-RestMethod`); in cmd.exe you get `irm is not recognized`.

```powershell
irm https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.ps1 | iex
```

It does the whole job:

1. installs the WSL components if absent (asking for admin once, **without** installing a Linux
   distribution) — if Windows needs a restart to finish, it says so and resumes automatically after;
2. downloads `vdb.exe` into `%LOCALAPPDATA%\Programs\vectoradb` and adds it to your PATH — including
   the current window, so `vdb` works immediately;
3. runs `vdb setup`, which imports the dedicated `vectoradb` distro, installs Docker, downloads and
   installs the OpenZFS module matching **your** WSL kernel, verifies `modprobe zfs` actually works,
   and brings the stack up.

Output is a short progress list; the full detail goes to
`%LOCALAPPDATA%\Programs\vectoradb\install.log`, which is named in any error.

If PowerShell blocks the script (`running scripts is disabled on this system`), allow it for this
session first, then re-run the install:

```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass
```

To install without running setup, or without the admin prompt, set `VDB_NO_SETUP=1` or
`VDB_NO_ELEVATE=1` before running the command.

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
| `The term '# ' is not recognized` at line 1 | A cached copy of an older `install.ps1` that had a UTF-8 BOM. The current installer is pure ASCII with no BOM, which is the only form that works both piped to `iex` and run as a file. Re-run to fetch the fixed copy. |
| `running scripts is disabled on this system` | `Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass`, then re-run the install. |
| `The request was aborted: The connection was closed unexpectedly` | Windows PowerShell 5.1 didn't negotiate TLS 1.2. The installer now sets it itself; if you hit this running an older copy, re-run the current one-liner. |
| `vdb is not recognized` after install | The installer adds it to the current window's PATH as well as persisting it, so this should not happen. In a window opened *before* installing: `$env:Path += ";$env:LOCALAPPDATA\Programs\vectoradb"`. |
| The install stops asking you to restart | Enabling the WSL components needs a reboot. Restart; the installer resumes on its own. If it doesn't, run the one-liner again. |
| `no ZFS bundle published for this WSL kernel` | Your WSL kernel is newer than any this VectoraDB release ships a module for. Check for a newer VectoraDB release, or build one with `make wsl-zfs`. Do **not** `wsl --update` — that moves the kernel further ahead. |
| `WSL is present but not healthy` | Enable virtualization in the BIOS and the *Virtual Machine Platform* feature; `wsl --update`. |
| `ZFS is not usable in the "vectoradb" distro` | The staged modules were built for a different kernel. Check `wsl -d vectoradb -- uname -r` against the bundle filename in `%LOCALAPPDATA%\Programs\vectoradb`. |
| `systemd did not finish booting` | `wsl --terminate vectoradb`, then re-run `vdb setup`. |
| `the VectoraDB ZFS pool device is not ready` | Deliberate stop: the pool could not be imported, and VectoraDB will not run the engine in case it recreates the pool over your data. Run `wsl --terminate vectoradb` and retry; if it persists, see `journalctl -u vectoradb-zpool.service` inside the distro. |
| `pool I/O is currently suspended` | The pool lost its backing device. `wsl --terminate vectoradb` and re-run `vdb setup`; the pool is re-imported at boot. |
| `wsl` commands hang and `wsl --shutdown` never returns | A suspended ZFS pool can wedge the WSL VM. In an **Administrator** PowerShell: `Restart-Service WSLService -Force` (a reboot also clears it). |

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
