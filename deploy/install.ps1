# SPDX-License-Identifier: Apache-2.0
#
# VectoraDB Windows installer: one command, from a machine with nothing on it to
# a running database. Installs WSL if absent (no Linux distribution required),
# installs the vdb launcher, and runs `vdb setup`.
#
# Usage (PowerShell):
#   irm https://raw.githubusercontent.com/vectoradb/vectoraDB/main/deploy/install.ps1 | iex
#
# This file must stay free of a UTF-8 BOM: `irm | iex` pipes the BOM into the
# parser, which then reports `The term '# ' is not recognized` on line 1.
#
# Env overrides: VDB_VERSION (default "latest"), VDB_REPO, VDB_PREFIX,
# VDB_NO_SETUP (skip `vdb setup`), VDB_NO_ELEVATE (never prompt for admin).

$ErrorActionPreference = 'Stop'

# Windows PowerShell 5.1 does not negotiate TLS 1.2 by default, and GitHub
# requires it -- without this, downloads fail with "the connection was closed
# unexpectedly" partway through.
try { [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12 } catch { }

$Repo    = if ($env:VDB_REPO)    { $env:VDB_REPO }    else { 'vectoradb/vectoraDB' }
$Version = if ($env:VDB_VERSION) { $env:VDB_VERSION } else { 'latest' }
$Prefix  = if ($env:VDB_PREFIX)  { $env:VDB_PREFIX }  else { "$env:LOCALAPPDATA\Programs\vectoradb" }

# Ubuntu publishes WSL rootfs tarballs directly; pulling from upstream keeps our
# own release small and avoids redistributing Ubuntu. Note the path: only
# /wsl/releases/<series>/current/ carries the tarballs -- /wsl/<series>/current/
# holds manifests alone.
$RootfsBase = 'https://cloud-images.ubuntu.com/wsl/releases/noble/current'
$RootfsName = 'ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz'
$RootfsUrl  = if ($env:VDB_ROOTFS_URL) { $env:VDB_ROOTFS_URL } else { "$RootfsBase/$RootfsName" }

$script:Step = 0
$script:Steps = 5

function Write-Step([string]$Message) {
    $script:Step++
    Write-Host ("  [{0}/{1}] {2}" -f $script:Step, $script:Steps, $Message)
}

# Resolve-Arch maps the OS architecture to our release-asset arch token.
function Resolve-Arch {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        default { 'amd64' }
    }
}

# Get-VdbAsset builds the download URL for a named release asset.
function Get-VdbAsset([string]$Name) {
    if ($Version -eq 'latest') {
        "https://github.com/$Repo/releases/latest/download/$Name"
    } else {
        "https://github.com/$Repo/releases/download/$Version/$Name"
    }
}

# Test-Admin reports whether this process is elevated.
function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal $id).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
}

# Get-WslKernelRelease reports the kernel WSL runs (`uname -r`), or $null when
# WSL is absent or has no distribution to run it in. wsl.exe emits UTF-16LE.
#
# The installer does not need this -- setup chooses the ZFS bundle -- but it is
# used to opportunistically pre-stage that bundle for older releases.
function Get-WslKernelRelease {
    if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) { return $null }
    try {
        $raw = & wsl.exe -e uname -r 2>$null
        $rel = ($raw -join '').Trim() -replace "`0", ''
        # A machine with WSL but no distro returns an error string, not a kernel.
        if ($rel -match '^[0-9]+\.[0-9]+') { return $rel }
    } catch { }
    return $null
}

# Test-WslReady reports whether WSL is installed and healthy enough to import a
# distro. `wsl --status` fails when the platform is present but not enabled.
function Test-WslReady {
    if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) { return $false }
    try {
        & wsl.exe --status *> $null
        return ($LASTEXITCODE -eq 0)
    } catch { return $false }
}

# Test-RebootPending reports whether Windows is waiting on a restart. Enabling
# the WSL optional components sets this on a machine that had them off.
function Test-RebootPending {
    $keys = @(
        'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Component Based Servicing\RebootPending',
        'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\WindowsUpdate\Auto Update\RebootRequired'
    )
    foreach ($k in $keys) { if (Test-Path $k) { return $true } }
    return $false
}

# Register-Resume arranges for this installer to run again after a reboot, so a
# machine that needed WSL enabled finishes on its own. It is a convenience, never
# the only path: re-running the one-liner by hand always works.
function Register-Resume {
    # RunOnce fires early at logon, typically before networking is up, so the
    # resumed command waits for the download host to answer before starting.
    # Without the wait it fails immediately on `irm` and the user sees only a
    # stray error window -- which is what happened on the first real machine
    # this was tried on.
    $url = 'https://raw.githubusercontent.com/vectoradb/vectoraDB/main/deploy/install.ps1'
    $cmd = "for (`$i=0; `$i -lt 60; `$i++) { " +
           "if (Test-Connection -ComputerName raw.githubusercontent.com -Count 1 -Quiet) { break }; " +
           "Start-Sleep -Seconds 5 }; irm $url | iex"
    $run = "powershell -NoExit -NoProfile -ExecutionPolicy Bypass -Command `"$cmd`""
    try {
        New-ItemProperty -Force -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\RunOnce' `
            -Name 'VectoraDBInstall' -Value $run -PropertyType String | Out-Null
        return $true
    } catch { return $false }
}

# Install-Wsl enables WSL without a Linux distribution.
#
# --no-distribution matters: VectoraDB imports its own dedicated distro, so an
# Ubuntu install is a pure waste of the user's time and disk. Requires admin, so
# this re-launches elevated and waits.
function Install-Wsl {
    if ($env:VDB_NO_ELEVATE) {
        throw "WSL is not installed. Run this in an Administrator PowerShell, then re-run the installer:`n" +
              "    wsl --install --no-distribution"
    }
    Write-Host "  VectoraDB needs WSL. Windows will ask for permission to install it."
    $wslArgs = @('--install', '--no-distribution')
    if (Test-Admin) {
        & wsl.exe --install --no-distribution 2>&1 | Out-String | Write-Verbose
    } else {
        $p = Start-Process -FilePath 'wsl.exe' -ArgumentList $wslArgs -Verb RunAs -Wait -PassThru
        if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) {
            throw "installing WSL failed (exit $($p.ExitCode)). Run 'wsl --install --no-distribution' in an Administrator PowerShell."
        }
    }
    try { & wsl.exe --update *> $null } catch { }
}

function Get-File([string]$Url, [string]$Dest, [bool]$Required = $true) {
    try {
        # Progress rendering dominates the runtime of a large download in
        # Windows PowerShell; suppressing it is worth several minutes on the
        # rootfs.
        $prev = $ProgressPreference
        $ProgressPreference = 'SilentlyContinue'
        try { Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $Dest }
        finally { $ProgressPreference = $prev }
        return $true
    } catch {
        if ($Required) { throw "download failed: $Url`n$($_.Exception.Message)" }
        Write-Warning "optional asset not in this release yet: $Url"
        return $false
    }
}

# Get-UpstreamChecksum returns the expected SHA256 for $Name from a publisher's
# SHA256SUMS listing, or $null if the listing can't be fetched or doesn't name
# the file. Callers decide what an unknown checksum means.
function Get-UpstreamChecksum([string]$SumsUrl, [string]$Name) {
    try {
        $body = (Invoke-WebRequest -UseBasicParsing -Uri $SumsUrl).Content
    } catch {
        Write-Warning "could not fetch $SumsUrl"
        return $null
    }
    # Windows PowerShell hands back a byte[] when the response isn't typed as
    # text; PowerShell 7 hands back a string. Normalize before splitting.
    $sums = if ($body -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($body) } else { [string]$body }
    # Lines look like: "<sha256>  *ubuntu-...rootfs.tar.gz" (or two spaces).
    foreach ($line in ($sums -split "`n")) {
        if ($line -match '^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$' -and $Matches[2] -eq $Name) {
            return $Matches[1].ToLower()
        }
    }
    Write-Warning "$Name not listed in SHA256SUMS"
    return $null
}

# Test-UpstreamChecksum reports whether a staged file still matches the
# publisher's listing. An unverifiable file is not reusable, so anything other
# than a positive match is $false.
function Test-UpstreamChecksum([string]$Path, [string]$SumsUrl, [string]$Name) {
    $want = Get-UpstreamChecksum $SumsUrl $Name
    if (-not $want) { return $false }
    return (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLower() -eq $want
}

# Assert-UpstreamChecksum verifies a download against the publisher's listing.
# Ubuntu's rootfs is a ~340 MB third-party download that becomes the root
# filesystem of a distro, so it is worth checking. A listing we cannot reach is
# a warning, not a failure -- the download itself already came over TLS.
function Assert-UpstreamChecksum([string]$Path, [string]$SumsUrl, [string]$Name) {
    $want = Get-UpstreamChecksum $SumsUrl $Name
    if (-not $want) {
        Write-Warning "skipping checksum verification for $Name"
        return
    }
    $got = (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLower()
    if ($got -ne $want) {
        Remove-Item $Path -Force -ErrorAction SilentlyContinue
        throw "checksum mismatch for $Name`n  expected $want`n  got      $got"
    }
}

# Add-ToPath appends a directory to the user PATH, idempotently.
function Add-ToPath([string]$Dir) {
    $cur = [Environment]::GetEnvironmentVariable('Path', 'User')
    $parts = @()
    if ($cur) { $parts = $cur -split ';' | Where-Object { $_ -ne '' } }
    if ($parts -notcontains $Dir) {
        $new = (@($parts + $Dir) -join ';')
        [Environment]::SetEnvironmentVariable('Path', $new, 'User')
        return $true
    }
    return $false
}

function Invoke-Install {
    $arch = Resolve-Arch
    if ($arch -ne 'amd64') {
        throw "unsupported architecture '$arch' -- only windows/amd64 is published (WSL2 runs x86_64)."
    }
    Write-Host ""
    Write-Host "VectoraDB installer" -ForegroundColor Cyan

    # 1. WSL. Done first because everything else is pointless without it -- and
    #    because it is the only step that can require a reboot.
    if (Test-WslReady) {
        Write-Step "WSL is already installed"
    } else {
        Write-Step "Installing WSL components (no Linux distribution needed)"
        Install-Wsl
        # Parenthesised deliberately: `Test-RebootPending -or ...` would pass
        # `-or` to the function as a parameter rather than combining them.
        if ((Test-RebootPending) -or -not (Test-WslReady)) {
            $resumed = Register-Resume
            Write-Host ""
            Write-Host "Windows needs to restart to finish enabling WSL." -ForegroundColor Yellow
            if ($resumed) {
                Write-Host "VectoraDB should continue by itself a moment after you sign back in."
            }
            # Always given, even when the resume was registered: it depends on
            # RunOnce firing and on networking being up, neither guaranteed.
            Write-Host "If it does not, just run the same command again:" -ForegroundColor Yellow
            Write-Host "    irm https://raw.githubusercontent.com/vectoradb/vectoraDB/main/deploy/install.ps1 | iex"
            Write-Host "Nothing is lost by re-running it -- the install picks up where it stopped."
            Write-Host ""
            return
        }
    }

    New-Item -ItemType Directory -Force -Path $Prefix | Out-Null

    # 2. The launcher and the engine binary.
    Write-Step "Downloading VectoraDB"
    Get-File (Get-VdbAsset 'vdb-windows-amd64.exe') "$Prefix\vdb.exe" | Out-Null
    Get-File (Get-VdbAsset 'vdb-linux-amd64') "$Prefix\vdb-linux-amd64" | Out-Null

    # The engine finds a Docker build context relative to the working directory,
    # which finds nothing for someone who installed vdb rather than cloning the
    # repo -- so ship the context and let `vdb setup` stage it into the distro.
    # tar.exe is built into Windows 10 1803+ and Windows 11.
    New-Item -ItemType Directory -Force -Path "$Prefix\docker-context" | Out-Null
    Get-File (Get-VdbAsset 'vectoradb-docker-context.tar.gz') "$Prefix\docker-context.tar.gz" | Out-Null
    & tar.exe -xzf "$Prefix\docker-context.tar.gz" -C "$Prefix\docker-context"
    if ($LASTEXITCODE -ne 0) { throw "could not expand the image build context (tar.exe failed)" }
    Remove-Item "$Prefix\docker-context.tar.gz" -Force

    # 3. The Ubuntu rootfs. ~340 MB, so don't re-fetch a verified copy.
    #
    # Choosing the ZFS bundle is `vdb setup`'s job: it is specific to the WSL
    # kernel, and only setup can know which one is needed, having just created
    # the distro -- whereas this script may run before WSL exists at all.
    #
    # It is still staged opportunistically below when WSL happens to be up, purely
    # so a new installer paired with an older release cannot produce a broken
    # install: a vdb.exe from before setup could fetch its own bundle would
    # otherwise find nothing and stop. Harmless when setup would fetch it anyway,
    # since a staged bundle is simply used as-is.
    $rootfs = "$Prefix\vectoradb-rootfs.tar.gz"
    $verify = -not $env:VDB_ROOTFS_URL
    if ((Test-Path $rootfs) -and $verify -and (Test-UpstreamChecksum $rootfs "$RootfsBase/SHA256SUMS" $RootfsName)) {
        Write-Step "Ubuntu rootfs already downloaded"
    } else {
        Write-Step "Downloading the Ubuntu rootfs (~340 MB, one time)"
        Get-File $RootfsUrl $rootfs | Out-Null
        if ($verify) { Assert-UpstreamChecksum $rootfs "$RootfsBase/SHA256SUMS" $RootfsName }
    }

    # Compatibility staging, per the note above. Best effort throughout: any
    # failure here is silent, because setup fetches the bundle itself.
    $rel = Get-WslKernelRelease
    if ($rel) {
        foreach ($name in @("vectoradb-zfs-modules-$rel.tar.gz", "vectoradb-zfs-$rel.tar.gz")) {
            if (Test-Path "$Prefix\$name") { break }
            if (Get-File (Get-VdbAsset $name) "$Prefix\$name" $false) { break }
            Remove-Item "$Prefix\$name" -Force -ErrorAction SilentlyContinue
        }
    }

    # 4. PATH -- persisted for new shells, and live in this one so `vdb` works
    #    immediately. Not doing the latter is why "vdb is not recognized" was
    #    the single most common complaint.
    Write-Step "Adding vdb to your PATH"
    Add-ToPath $Prefix | Out-Null
    if (($env:Path -split ';') -notcontains $Prefix) { $env:Path = "$env:Path;$Prefix" }

    # 5. Finish the job. An installer that stops here and tells the user to run
    #    another command is where most installs died.
    if ($env:VDB_NO_SETUP) {
        Write-Step "Skipping setup (VDB_NO_SETUP)"
        Write-Host ""
        Write-Host "Installed. Run:  vdb setup" -ForegroundColor Green
        return
    }
    Write-Step "Setting up VectoraDB (first run downloads Docker and ZFS)"
    Write-Host ""
    & "$Prefix\vdb.exe" setup
    if ($LASTEXITCODE -ne 0) {
        throw "vdb setup failed. See $Prefix\install.log, then re-run:  vdb setup"
    }

    Write-Host ""
    Write-Host "VectoraDB is running." -ForegroundColor Green
    Write-Host "  Try:  vdb status"
    Write-Host "  Log:  $Prefix\install.log"
}

# Run only when executed/piped -- not when dot-sourced by tests.
if ($MyInvocation.InvocationName -ne '.') {
    Invoke-Install
}
