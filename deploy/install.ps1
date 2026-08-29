# SPDX-License-Identifier: Apache-2.0
#
# VectoraDB Windows installer. Installs the native launcher (vdb.exe) plus the
# support assets `vdb setup` needs to build the WSL2 engine: the Linux engine
# binary, an Ubuntu rootfs, the Postgres image build context, and the OpenZFS
# modules built for the WSL2 kernel this machine runs.
#
# Usage (PowerShell):
#   irm https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.ps1 | iex
#
# Env overrides: VDB_VERSION (default "latest"), VDB_REPO, VDB_PREFIX.

$ErrorActionPreference = 'Stop'

$Repo    = if ($env:VDB_REPO)    { $env:VDB_REPO }    else { 'SauravYadav12/vectoraDB' }
$Version = if ($env:VDB_VERSION) { $env:VDB_VERSION } else { 'latest' }
$Prefix  = if ($env:VDB_PREFIX)  { $env:VDB_PREFIX }  else { "$env:LOCALAPPDATA\Programs\vectoradb" }

# Ubuntu publishes WSL rootfs tarballs directly; pulling from upstream keeps our
# own release small and avoids redistributing Ubuntu. Note the path: only
# /wsl/releases/<series>/current/ carries the tarballs — /wsl/<series>/current/
# holds manifests alone.
$RootfsBase = 'https://cloud-images.ubuntu.com/wsl/releases/noble/current'
$RootfsName = 'ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz'
$RootfsUrl  = if ($env:VDB_ROOTFS_URL) { $env:VDB_ROOTFS_URL } else { "$RootfsBase/$RootfsName" }

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

# Get-WslKernelRelease reports the kernel WSL runs (`uname -r`), which is what the
# ZFS modules must be built against. wsl.exe emits UTF-16LE, hence the decode.
# Returns $null when WSL isn't installed or isn't healthy.
function Get-WslKernelRelease {
    if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) { return $null }
    try {
        $raw = & wsl.exe -e uname -r 2>$null
        $rel = ($raw -join '').Trim() -replace "`0", ''
        if ($rel) { return $rel }
    } catch { }
    return $null
}

# Get-ZfsBundleName names the ZFS artifact for a kernel release. The release is
# part of the filename so a stale bundle is a missing download, not a module that
# silently refuses to load.
function Get-ZfsBundleName([string]$KernelRelease) {
    "vectoradb-zfs-$($KernelRelease.Trim()).tar.gz"
}

# Add-ToPath appends a directory to the user PATH, idempotently.
function Add-ToPath([string]$Dir) {
    $cur = [Environment]::GetEnvironmentVariable('Path', 'User')
    $parts = @()
    if ($cur) { $parts = $cur -split ';' | Where-Object { $_ -ne '' } }
    if ($parts -notcontains $Dir) {
        $new = (@($parts + $Dir) -join ';')
        [Environment]::SetEnvironmentVariable('Path', $new, 'User')
        $env:Path = "$env:Path;$Dir"
        return $true
    }
    return $false
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
# a warning, not a failure — the download itself already came over TLS.
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
    Write-Host "  checksum OK"
}

function Get-File([string]$Url, [string]$Dest, [bool]$Required = $true) {
    try {
        Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $Dest
        return $true
    } catch {
        if ($Required) { throw "download failed: $Url`n$($_.Exception.Message)" }
        Write-Warning "optional asset not in this release yet: $Url"
        return $false
    }
}

function Invoke-Install {
    $arch = Resolve-Arch
    if ($arch -ne 'amd64') {
        throw "unsupported architecture '$arch' — only windows/amd64 is published (WSL2 runs x86_64)."
    }
    New-Item -ItemType Directory -Force -Path $Prefix | Out-Null

    Write-Host "Downloading vdb.exe ($Version)…"
    Get-File (Get-VdbAsset 'vdb-windows-amd64.exe') "$Prefix\vdb.exe" | Out-Null

    Write-Host "Staging the Linux engine binary…"
    Get-File (Get-VdbAsset 'vdb-linux-amd64') "$Prefix\vdb-linux-amd64" | Out-Null

    # The engine finds a Docker build context relative to the working directory,
    # which finds nothing for someone who installed vdb rather than cloning the
    # repo — so ship the context and let `vdb setup` stage it into the distro.
    # tar.exe is built into Windows 10 1803+ and Windows 11.
    Write-Host "Staging the Postgres image build context…"
    New-Item -ItemType Directory -Force -Path "$Prefix\docker-context" | Out-Null
    Get-File (Get-VdbAsset 'vectoradb-docker-context.tar.gz') "$Prefix\docker-context.tar.gz" | Out-Null
    & tar.exe -xzf "$Prefix\docker-context.tar.gz" -C "$Prefix\docker-context"
    if ($LASTEXITCODE -ne 0) { throw "could not expand the image build context (tar.exe failed)" }
    Remove-Item "$Prefix\docker-context.tar.gz" -Force

    # ~340 MB, so don't re-fetch it when a verified copy is already staged
    # (re-running the installer to pick up a new vdb.exe is common).
    $rootfs = "$Prefix\vectoradb-rootfs.tar.gz"
    $verify = -not $env:VDB_ROOTFS_URL
    if ((Test-Path $rootfs) -and $verify -and (Test-UpstreamChecksum $rootfs "$RootfsBase/SHA256SUMS" $RootfsName)) {
        Write-Host "Ubuntu WSL rootfs already staged (checksum OK)."
    } else {
        Write-Host "Downloading the Ubuntu WSL rootfs (~340 MB)…"
        Get-File $RootfsUrl $rootfs | Out-Null
        if ($verify) { Assert-UpstreamChecksum $rootfs "$RootfsBase/SHA256SUMS" $RootfsName }
    }

    # The ZFS modules are kernel-specific. Fetch the bundle matching this
    # machine's WSL kernel; if WSL isn't up yet we can't know it, and `vdb setup`
    # will name the exact missing file after WSL is installed.
    $rel = Get-WslKernelRelease
    if ($rel) {
        Write-Host "Staging ZFS for WSL kernel $rel…"
        $bundle = Get-ZfsBundleName $rel
        if (-not (Get-File (Get-VdbAsset $bundle) "$Prefix\$bundle" $false)) {
            Write-Warning "No ZFS bundle published for kernel $rel."
            Write-Warning "Your WSL kernel is newer than this VectoraDB release; run 'wsl --update' is not the fix —"
            Write-Warning "check for a newer VectoraDB release, or build it yourself with 'make wsl-zfs'."
        }
    } else {
        Write-Warning "WSL not detected — skipping the ZFS bundle."
        Write-Warning "Install WSL ('wsl --install' in an Administrator PowerShell, then reboot) and re-run this installer."
    }

    $added = Add-ToPath $Prefix
    Write-Host ""
    Write-Host "Installed vdb to $Prefix\vdb.exe" -ForegroundColor Green
    if ($added) { Write-Host "Added $Prefix to your PATH (open a new terminal to pick it up)." }
    Write-Host ""
    Write-Host "Next: run  vdb setup"
    Write-Host "  (creates the 'vectoradb' WSL distro, installs Docker + ZFS in it, and brings the database up)"
    Write-Host "  Your other WSL distros are not touched."
}

# Run only when executed/piped — not when dot-sourced by tests.
if ($MyInvocation.InvocationName -ne '.') {
    Invoke-Install
}
