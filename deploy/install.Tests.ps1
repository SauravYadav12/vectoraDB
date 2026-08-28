# SPDX-License-Identifier: Apache-2.0
# Pester tests for install.ps1 (TC4.4). Dot-sourcing install.ps1 loads its
# functions without running the installer (guarded by $MyInvocation.InvocationName).
# Run: Invoke-Pester deploy/install.Tests.ps1

Describe 'Resolve-Arch' {
    BeforeAll { . "$PSScriptRoot/install.ps1" }
    It 'returns a known arch token' {
        Resolve-Arch | Should -BeIn @('amd64', 'arm64')
    }
}

Describe 'Get-VdbAsset (latest)' {
    BeforeAll {
        $env:VDB_VERSION = $null
        . "$PSScriptRoot/install.ps1"
    }
    It 'builds a latest release URL' {
        Get-VdbAsset 'vdb-windows-amd64.exe' |
            Should -Be 'https://github.com/SauravYadav12/vectoraDB/releases/latest/download/vdb-windows-amd64.exe'
    }
}

Describe 'Get-VdbAsset (pinned)' {
    BeforeAll {
        $env:VDB_VERSION = 'v1.2.3'
        . "$PSScriptRoot/install.ps1"
    }
    AfterAll { $env:VDB_VERSION = $null }
    It 'builds a versioned release URL' {
        Get-VdbAsset 'vdb-windows-amd64.exe' |
            Should -Be 'https://github.com/SauravYadav12/vectoraDB/releases/download/v1.2.3/vdb-windows-amd64.exe'
    }
}

Describe 'Add-ToPath' {
    BeforeAll { . "$PSScriptRoot/install.ps1" }
    It 'is idempotent for an already-present dir' {
        $existing = ([Environment]::GetEnvironmentVariable('Path', 'User') -split ';' |
            Where-Object { $_ -ne '' } | Select-Object -First 1)
        if ($existing) { Add-ToPath $existing | Should -BeFalse }
    }
}

# TC4.6 — the ZFS bundle is named for the kernel it was built against, so an
# installer running against a bumped WSL kernel misses the download rather than
# staging modules that cannot load. Must agree with zfsBundleName in
# internal/host/host_wsl.go.
Describe 'Get-ZfsBundleName' {
    BeforeAll { . "$PSScriptRoot/install.ps1" }
    It 'embeds the kernel release in the filename' {
        Get-ZfsBundleName '6.6.87.2-microsoft-standard-WSL2' |
            Should -Be 'vectoradb-zfs-6.6.87.2-microsoft-standard-WSL2.tar.gz'
    }
    It 'trims surrounding whitespace from the release' {
        Get-ZfsBundleName "  6.6.87.2-microsoft-standard-WSL2`n" |
            Should -Be 'vectoradb-zfs-6.6.87.2-microsoft-standard-WSL2.tar.gz'
    }
}

# TC4.7 — kernel detection must degrade to $null rather than throw when WSL is
# absent (the CI runner has no WSL), so the installer can warn and continue.
Describe 'Get-WslKernelRelease' {
    BeforeAll { . "$PSScriptRoot/install.ps1" }
    It 'returns null or a non-empty release string, never throws' {
        $rel = Get-WslKernelRelease
        if ($null -ne $rel) { $rel | Should -Not -BeNullOrEmpty }
    }
}

# TC4.8 — Windows ships tar.exe (10 1803+/11); the installer uses it to expand
# the image build context, so a missing tar is a hard install failure.
Describe 'tar.exe availability' {
    It 'is present on PATH' {
        Get-Command tar.exe -ErrorAction SilentlyContinue | Should -Not -BeNullOrEmpty
    }
}
