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
