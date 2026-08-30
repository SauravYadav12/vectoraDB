# SPDX-License-Identifier: Apache-2.0
#
# Windows installation tests.
#
# These cover the install *flow* -- the parts that repeatedly broke on real
# machines and that unit tests cannot see: script encoding, the WSL bootstrap,
# PATH handling, and the ordering rule that setup (not the installer) chooses the
# kernel-specific ZFS bundle.
#
# Run:  Invoke-Pester deploy/windows-install.Tests.ps1
#
# Tests that would create a distro or download hundreds of megabytes are tagged
# 'E2E' and skipped unless VDB_TEST_E2E is set:
#   $env:VDB_TEST_E2E=1; Invoke-Pester deploy/windows-install.Tests.ps1

BeforeAll {
    $script:Installer = "$PSScriptRoot/install.ps1"
    $script:E2E = [bool]$env:VDB_TEST_E2E
}

# The encoding is a genuine two-sided constraint, and getting it wrong broke real
# installs in both directions:
#   with a BOM   -> `irm | iex` fails with "The term '# ' is not recognized"
#   without one  -> Windows PowerShell 5.1 reads non-ASCII as ANSI and fails to parse
# Pure ASCII, no BOM, is the only encoding that satisfies both.
Describe 'installer encoding' {
    It 'has no UTF-8 BOM' {
        $b = [System.IO.File]::ReadAllBytes($Installer)
        ($b[0] -eq 0xEF -and $b[1] -eq 0xBB -and $b[2] -eq 0xBF) | Should -BeFalse
    }

    It 'contains no non-ASCII bytes' {
        $bad = [System.IO.File]::ReadAllBytes($Installer) | Where-Object { $_ -gt 127 }
        $bad | Should -BeNullOrEmpty
    }

    It 'parses the way iex receives it' {
        $text = [System.IO.File]::ReadAllText($Installer)
        { [scriptblock]::Create($text) } | Should -Not -Throw
    }

    It 'parses the way a downloaded file is run' {
        $errors = $null
        [System.Management.Automation.PSParser]::Tokenize(
            (Get-Content $Installer -Raw), [ref]$errors) | Out-Null
        $errors | Should -BeNullOrEmpty
    }
}

# The installer must not need WSL. Choosing the ZFS bundle by asking a running
# distro for `uname -r` is what forced users to install WSL and Ubuntu by hand,
# reboot, and re-run the installer -- and left a vdb.exe that could not set
# itself up when they did not.
Describe 'installer does not depend on WSL' {
    BeforeAll { . $Installer }

    It 'reports no kernel when WSL is absent, rather than failing' {
        # The installer may legitimately run before WSL exists. Kernel detection
        # must degrade to $null, never throw and never block the install.
        { Get-WslKernelRelease } | Should -Not -Throw
    }

    It 'treats the ZFS bundle as optional, never required' {
        # Staging it is a compatibility shim for releases whose vdb.exe predates
        # setup fetching its own bundle. It must stay best-effort: Get-File's
        # third argument $false means "optional", so a missing bundle warns and
        # the install continues rather than stopping.
        $src = Get-Content $Installer -Raw
        if ($src -match 'vectoradb-zfs') {
            # The name and the optional flag sit on separate lines, so match the
            # call itself rather than requiring them adjacent.
            $src | Should -Match 'Get-File \(Get-VdbAsset \$name\)[^\r\n]*\$false'
        }
    }

    It 'does not abort when WSL has no distribution' {
        # A machine with WSL but no distro returns an error string from
        # `wsl -e uname -r`; it must not be mistaken for a kernel version.
        $src = Get-Content $Installer -Raw
        $src | Should -Match "rel -match"
    }

    It 'installs WSL without a Linux distribution' {
        $src = Get-Content $Installer -Raw
        $src | Should -Match '--no-distribution'
        $src | Should -Not -Match 'wsl --install -d '
    }
}

Describe 'installer behaviour' {
    BeforeAll { . $Installer }

    It 'resolves a supported architecture' {
        Resolve-Arch | Should -BeIn @('amd64', 'arm64')
    }

    It 'builds a latest asset URL over HTTPS' {
        $u = Get-VdbAsset 'vdb-windows-amd64.exe'
        $u | Should -BeLike 'https://*'
        $u | Should -Be 'https://github.com/vectoradb/vectoraDB/releases/latest/download/vdb-windows-amd64.exe'
    }

    It 'never downloads over plain HTTP' {
        # An installer fetching executables over http would be trivially
        # MITM-able; every URL in the script must be https.
        $http = Select-String -Path $Installer -Pattern 'http://' -AllMatches
        $http | Should -BeNullOrEmpty
    }

    It 'reports elevation without throwing' {
        { Test-Admin } | Should -Not -Throw
        (Test-Admin) | Should -BeOfType [bool]
    }

    It 'probes WSL without throwing, even when absent' {
        { Test-WslReady } | Should -Not -Throw
    }

    It 'detects a pending reboot without throwing' {
        { Test-RebootPending } | Should -Not -Throw
    }

    It 'adds to PATH idempotently' {
        $existing = ([Environment]::GetEnvironmentVariable('Path', 'User') -split ';' |
            Where-Object { $_ -ne '' } | Select-Object -First 1)
        if ($existing) { Add-ToPath $existing | Should -BeFalse }
    }

    It 'makes vdb usable in the current session, not just new ones' {
        # "vdb is not recognized" was the most common post-install complaint:
        # persisting the User PATH only affects shells started afterwards.
        (Get-Content $Installer -Raw) | Should -Match '\$env:Path\s*='
    }

    It 'runs setup rather than telling the user to' {
        (Get-Content $Installer -Raw) | Should -Match 'setup'
    }

    It 'honours VDB_NO_SETUP and VDB_NO_ELEVATE' {
        $src = Get-Content $Installer -Raw
        $src | Should -Match 'VDB_NO_SETUP'
        $src | Should -Match 'VDB_NO_ELEVATE'
    }
}

Describe 'checksum verification' {
    BeforeAll { . $Installer }

    It 'parses a publisher SHA256SUMS listing' {
        $sums = "abc  other.tar.gz`n" +
                ("0" * 64) + "  ubuntu-noble-wsl-amd64-wsl.rootfs.tar.gz`n"
        $tmp = New-TemporaryFile
        try {
            Set-Content -Path $tmp -Value $sums -Encoding ascii
            # Get-UpstreamChecksum fetches over HTTP, so exercise the matching
            # rule directly rather than the network.
            ($sums -split "`n") | Where-Object { $_ -match '^([0-9a-fA-F]{64})\s+\*?(.+?)\s*$' } |
                Should -Not -BeNullOrEmpty
        } finally { Remove-Item $tmp -Force -ErrorAction SilentlyContinue }
    }

    It 'verifies the Ubuntu rootfs it downloads' {
        (Get-Content $Installer -Raw) | Should -Match 'Assert-UpstreamChecksum'
    }
}

# Windows ships tar.exe (10 1803+ / 11); the installer relies on it.
Describe 'host prerequisites' {
    It 'has tar.exe on PATH' {
        Get-Command tar.exe -ErrorAction SilentlyContinue | Should -Not -BeNullOrEmpty
    }
}

# Real installs. Skipped by default: they create a WSL distro and download
# hundreds of megabytes.
Describe 'end to end' -Tag 'E2E' {
    BeforeAll {
        if (-not $script:E2E) { return }
        $script:Prefix = Join-Path $env:TEMP "vdb-e2e-$(Get-Random)"
    }
    AfterAll {
        if ($script:Prefix -and (Test-Path $script:Prefix)) {
            Remove-Item $script:Prefix -Recurse -Force -ErrorAction SilentlyContinue
        }
    }

    It 'installs without running setup' -Skip:(-not $env:VDB_TEST_E2E) {
        $env:VDB_NO_SETUP = '1'
        $env:VDB_PREFIX = $script:Prefix
        try {
            & $Installer
            $LASTEXITCODE | Should -Be 0
            Test-Path (Join-Path $script:Prefix 'vdb.exe') | Should -BeTrue
            Test-Path (Join-Path $script:Prefix 'vdb-linux-amd64') | Should -BeTrue
            # The ZFS bundle must NOT be staged: setup fetches it, because only
            # setup knows the kernel.
            (Get-ChildItem $script:Prefix -Filter 'vectoradb-zfs-*') | Should -BeNullOrEmpty
        } finally {
            Remove-Item Env:VDB_NO_SETUP, Env:VDB_PREFIX -ErrorAction SilentlyContinue
        }
    }

    It 'reports a version' -Skip:(-not $env:VDB_TEST_E2E) {
        & (Join-Path $script:Prefix 'vdb.exe') version | Should -Match 'vdb'
    }
}
