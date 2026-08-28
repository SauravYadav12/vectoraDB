# SPDX-License-Identifier: Apache-2.0
#
# VectoraDB Windows installer. Installs the native launcher (vdb.exe) plus the
# support assets `vdb setup` needs to build the WSL2 engine (the Linux engine
# binary, the ZFS-enabled WSL2 kernel, and an Ubuntu rootfs).
#
# Usage (PowerShell):
#   irm https://raw.githubusercontent.com/SauravYadav12/vectoraDB/main/deploy/install.ps1 | iex
#
# Env overrides: VDB_VERSION (default "latest"), VDB_REPO, VDB_PREFIX.

$ErrorActionPreference = 'Stop'

$Repo    = if ($env:VDB_REPO)    { $env:VDB_REPO }    else { 'SauravYadav12/vectoraDB' }
$Version = if ($env:VDB_VERSION) { $env:VDB_VERSION } else { 'latest' }
$Prefix  = if ($env:VDB_PREFIX)  { $env:VDB_PREFIX }  else { "$env:LOCALAPPDATA\Programs\vectoradb" }

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

    # These come from the WSL2-kernel build (see docs/windows-setup.md). Optional
    # here so vdb.exe still installs before that pipeline lands; `vdb setup`
    # reports clearly if they're missing.
    Write-Host "Staging the ZFS-enabled WSL2 kernel + Ubuntu rootfs…"
    Get-File (Get-VdbAsset 'vectoradb-wsl-kernel') "$Prefix\vectoradb-wsl-kernel" $false | Out-Null
    Get-File (Get-VdbAsset 'vectoradb-rootfs.tar') "$Prefix\vectoradb-rootfs.tar" $false | Out-Null

    $added = Add-ToPath $Prefix
    Write-Host ""
    Write-Host "Installed vdb to $Prefix\vdb.exe" -ForegroundColor Green
    if ($added) { Write-Host "Added $Prefix to your PATH (open a new terminal to pick it up)." }
    Write-Host ""
    Write-Host "Next: run  vdb setup"
    Write-Host "  (installs the WSL2 distro + ZFS kernel, Docker, and brings the database up)"
}

# Run only when executed/piped — not when dot-sourced by tests.
if ($MyInvocation.InvocationName -ne '.') {
    Invoke-Install
}
