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
            Should -Be 'https://github.com/vectoradb/vectoraDB/releases/latest/download/vdb-windows-amd64.exe'
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
            Should -Be 'https://github.com/vectoradb/vectoraDB/releases/download/v1.2.3/vdb-windows-amd64.exe'
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
# TC4.6 -- the installer must not depend on WSL. Choosing the ZFS bundle moved
# into `vdb setup`, which is the first point the right kernel is knowable; an
# installer that needed WSL first is what forced the old manual multi-step setup.
Describe 'installer is independent of WSL' {
    BeforeAll { . "$PSScriptRoot/install.ps1" }
    It 'no longer chooses the ZFS bundle itself' {
        # Choosing it needs the WSL kernel, which the installer cannot know
        # before WSL exists; setup does it. Kernel detection survives only as a
        # best-effort pre-stage for older releases.
        Get-Command Get-ZfsBundleName -ErrorAction SilentlyContinue | Should -BeNullOrEmpty
    }

    It 'degrades gracefully when WSL is absent' {
        { Get-WslKernelRelease } | Should -Not -Throw
    }
    It 'never mentions a distribution when installing WSL' {
        $src = Get-Content "$PSScriptRoot/install.ps1" -Raw
        $src | Should -Match '--no-distribution'
        $src | Should -Not -Match 'wsl --install -d '
    }
}

# TC4.7 -- encoding. These two invocations pull in opposite directions: a BOM
# breaks `irm | iex` ("The term '# ' is not recognized"), and no BOM makes
# Windows PowerShell 5.1 read non-ASCII as ANSI and fail to parse the file.
# Pure ASCII with no BOM is the only encoding that satisfies both.
Describe 'install.ps1 encoding' {
    It 'has no UTF-8 BOM' {
        $b = [System.IO.File]::ReadAllBytes("$PSScriptRoot/install.ps1")
        ($b[0] -eq 0xEF -and $b[1] -eq 0xBB -and $b[2] -eq 0xBF) | Should -BeFalse
    }
    It 'is pure ASCII' {
        $bad = [System.IO.File]::ReadAllBytes("$PSScriptRoot/install.ps1") | Where-Object { $_ -gt 127 }
        $bad | Should -BeNullOrEmpty
    }
    It 'parses the way iex would receive it' {
        $text = [System.IO.File]::ReadAllText("$PSScriptRoot/install.ps1")
        { [scriptblock]::Create($text) } | Should -Not -Throw
    }
}

# TC4.8 -- Windows ships tar.exe (10 1803+/11); the installer uses it to expand
# the image build context, so a missing tar is a hard install failure.
Describe 'tar.exe availability' {
    It 'is present on PATH' {
        Get-Command tar.exe -ErrorAction SilentlyContinue | Should -Not -BeNullOrEmpty
    }
}
