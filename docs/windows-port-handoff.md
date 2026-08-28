# Windows Port — Handoff for the Windows machine

This branch (`feat/windows-support`) adds native Windows support to VectoraDB. All the **code** is
written and everything that can be verified on macOS/Linux is green. The remaining work **requires a
real Windows 11 machine** (and, for the kernel, a Linux builder or CI). This doc tells the next
Claude instance exactly what is done, what is left, and how to finish it.

> Read `docs/windows-setup.md` (the user-facing guide) and the local plan behind this work first.
> Architecture in one line: the engine is Linux-only; on Windows `vdb.exe` forwards every engine
> command into a dedicated **WSL2** distro named `vectoradb` — the exact analog of the macOS Lima VM.
> The launcher marks forwarded processes with `VECTORADB_IN_GUEST=1` so the in-distro `vdb` runs the
> engine in-process.

## What is DONE and verified (on macOS)

- **Build unblock.** `internal/daemon` was split with the repo's first build tags:
  `daemon.go` (shared) + `daemon_unix.go` (`//go:build !windows`) + `daemon_windows.go` (stubs).
  `GOOS=windows GOARCH=amd64 go build ./cmd/vdb` now succeeds.
- **Launcher.** `internal/host` is split into:
  - `host.go` — shared dispatch (`Maybe`, `Setup`, `hostForward`, `importLocalFile`, `bundledLinuxBinary`).
  - `host_darwin.go` — the existing Lima path (unchanged behavior, moved).
  - `host_windows.go` — the **WSL2 path**: `forward`/`forwardStdin` via `wsl.exe`, `setupWindows`,
    `ensureKernel`, `importDistro`, `provisionGuestWSL`, `installGuestBinaryWSL`.
  - `host_other.go` — Linux/other.
  - `host_wsl.go` — **pure, unit-tested** helpers: `wslArgs`, `decodeWSLList` (UTF-16LE),
    `mergeWslConfig`, `winPathToMnt`, `resolveWSLDistro`. Tests in `host_wsl_test.go` pass on any OS.
- **Release + installer.** `make release` builds `dist/vdb-windows-amd64.exe`. `deploy/install.ps1`
  installs it + stages assets; `deploy/install.Tests.ps1` are Pester tests.
- **Kernel build scripts (not yet run):** `deploy/wsl-kernel/build.sh` + `build-rootfs.sh`, `make wsl-kernel`.
- **Docs/CI:** `docs/windows-setup.md`, README Windows section, `.github/workflows/windows-ci.yml`.

Green on macOS: `go build ./...`, `go vet ./...`, `go test ./...`, `gofmt -l`, all five cross-compile
targets, and the macOS launcher still forwards to Lima (regression-checked).

## What is LEFT (do this on the Windows machine)

### Step 1 — Build the ZFS-enabled WSL2 kernel + rootfs assets
The stock WSL2 kernel has **no zfs module**, which the engine needs. Two artifacts are required and
are **not** in the repo (they're large binaries built by CI/a Linux builder):
- `vectoradb-wsl-kernel` — a WSL2 `bzImage` built with OpenZFS modules.
- `vectoradb-rootfs.tar` — an Ubuntu WSL rootfs with the **matching** zfs modules baked into
  `/lib/modules/<kernelrelease>/` (so `modprobe zfs` works against our kernel).

Build them on a Linux box (or WSL2 Ubuntu on this machine) with:
```bash
make wsl-kernel      # runs deploy/wsl-kernel/build.sh then build-rootfs.sh → dist/
```
Pin versions live at the top of `deploy/wsl-kernel/build.sh` (`KERNEL_TAG`, `ZFS_TAG`). **Verify the
kernel release string of the kernel matches the `/lib/modules/<rel>` baked into the rootfs** — this
is the #1 thing that breaks `modprobe zfs`.

Then place both assets (plus `vdb-linux-amd64` and `vdb.exe`) next to the installed `vdb.exe`
(`%LOCALAPPDATA%\Programs\vectoradb`) — or attach them to a GitHub release so `install.ps1` fetches
them. `bundledAsset()` in `host_windows.go` looks for them next to the exe / in `.\dist`.

### Step 2 — Build/stage the Windows binary locally
```powershell
# from a checkout of this branch, with Go installed:
$env:GOOS='windows'; $env:GOARCH='amd64'; go build -o $env:LOCALAPPDATA\Programs\vectoradb\vdb.exe .\cmd\vdb
# stage the guest engine binary (build it in WSL2 or cross-compile):
#   GOOS=linux GOARCH=amd64 go build -o vdb-linux-amd64 ./cmd/vdb
# copy vdb-linux-amd64, vectoradb-wsl-kernel, vectoradb-rootfs.tar next to vdb.exe
```

### Step 3 — Run `vdb setup` and debug the WSL2 path end-to-end
```powershell
vdb setup
```
This exercises `setupWindows` → `ensureKernel` (writes `%UserProfile%\.wslconfig`, `wsl --shutdown`)
→ `importDistro` (`wsl --import vectoradb …`, enables systemd) → `provisionGuestWSL`
(`apt-get install zfsutils-linux docker.io`, `modprobe zfs`) → `installGuestBinaryWSL` → `vdb start`.
**Expect to fix real issues here** — this code compiles but has never executed. Likely spots:
- `wsl.exe` UTF-16 output parsing (`decodeWSLOutput`) on this machine's locale.
- systemd readiness timing after `wsl --terminate` (may need a wait/retry before `systemctl`).
- Whether the imported rootfs's default user is root (setup runs `wsl -u root`); confirm.
- `.wslconfig` kernel path escaping (we write double-backslash Windows paths).
- localhost forwarding of ports 6432/8080/8088/9001 from Windows.

### Step 4 — Run the [W-E2E] acceptance test cases
From the plan's test matrix (the `[W-E2E]` ones). Minimum acceptance:
```
vdb setup                    # TC2.*  — distro created, kernel applied, stack up
vdb status                   # main ready
vdb branch create demo       # TC3.4  — zfs clone works (copy-on-write on Windows)
# connect a Windows psql/client to localhost:6432 (gateway), run CRUD
vdb import --from C:\path\to\data.csv   # stdin streaming across the Windows→WSL boundary
vdb import --from <postgres|mysql|mongo dsn>   # + --continuous + import-cutover
vdb branch delete demo
vdb stop
```
Capture outputs into `docs/windows-setup.md`. Also confirm TC3.2 (`uname -r` = our kernel,
`modprobe zfs` ok, `zfs version`) and TC3.3 (a stock distro still boots on the new global kernel).

## Key files (where to work)
- `internal/host/host_windows.go` — the WSL2 orchestration you'll iterate on.
- `internal/host/host_wsl.go` (+ `_test.go`) — pure helpers; add cases here as you learn `wsl.exe` quirks.
- `deploy/wsl-kernel/build.sh`, `build-rootfs.sh` — kernel/rootfs build.
- `deploy/install.ps1` — installer; `deploy/install.Tests.ps1` — Pester.
- `docs/windows-setup.md` — user guide to keep in sync.

## Guardrails / notes
- **Do not change the engine** (`internal/branch`, `internal/proxy`, etc.) — it runs unchanged in WSL2.
  If something's wrong, it's almost certainly in `host_windows.go` or the kernel/rootfs assets.
- Keep side-effecting `wsl.exe`/`.wslconfig` calls thin; put logic in pure helpers in `host_wsl.go`
  and unit-test it (that's why TC1.* run without a Windows host).
- `.wslconfig kernel=` is machine-wide; the ZFS kernel is a superset so other distros keep working —
  say so to the user, don't surprise them.
- Verify `go test ./internal/host` still passes on Windows (native) — TC1.* should be green there too.
