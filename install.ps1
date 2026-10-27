# xlfp installer (Windows PowerShell): downloads the matching GitHub
# Release archive, verifies its SHA-256 checksum, and installs xlfp.exe.
#
#   irm https://raw.githubusercontent.com/tyk-swe/xlfp/main/install.ps1 | iex
#
# Environment knobs:
#   XLFP_VERSION          release tag to install (default: latest)
#   XLFP_INSTALL_DIR      destination directory (default: %LOCALAPPDATA%\xlfp)
#   XLFP_RELEASE_BASE_URL download base override (testing/mirrors)
#
# While the repository is private, downloads fall back to the GitHub
# CLI (`gh release download`) when plain HTTPS gets a 404; anonymous
# download works once the repository is public.
$ErrorActionPreference = 'Stop'

$repo = 'tyk-swe/xlfp'
$version = $env:XLFP_VERSION
$installDir = if ($env:XLFP_INSTALL_DIR) { $env:XLFP_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'xlfp' }

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "xlfp install: unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}
$asset = "xlfp_windows_$arch.zip"

$tag = ''
if ($version) {
    $tag = if ($version.StartsWith('v')) { $version } else { "v$version" }
}

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("xlfp-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading xlfp for windows/$arch..."
    $downloaded = $false
    try {
        if ($env:XLFP_RELEASE_BASE_URL) {
            $base = $env:XLFP_RELEASE_BASE_URL
        } elseif ($tag) {
            $base = "https://github.com/$repo/releases/download/$tag"
        } else {
            $base = "https://github.com/$repo/releases/latest/download"
        }
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile (Join-Path $tmp $asset)
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')
        $downloaded = $true
    } catch {
        if (Get-Command gh -ErrorAction SilentlyContinue) {
            if ($tag) {
                gh release download $tag --repo $repo --pattern $asset --pattern 'checksums.txt' --dir $tmp --clobber
            } else {
                gh release download --repo $repo --pattern $asset --pattern 'checksums.txt' --dir $tmp --clobber
            }
            if ($LASTEXITCODE -eq 0) { $downloaded = $true }
        }
    }
    if (-not $downloaded) {
        throw "xlfp install: download failed. If the repository is still private, install the GitHub CLI, run ``gh auth login``, and retry - or download a release archive manually from https://github.com/$repo/releases"
    }

    $line = Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { $_ -match "\s+$([regex]::Escape($asset))$" } | Select-Object -First 1
    if (-not $line) { throw "xlfp install: checksums.txt has no entry for $asset" }
    $expected = ($line -split '\s+')[0].ToLower()
    $actual = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $asset)).Hash.ToLower()
    if ($actual -ne $expected) {
        throw "xlfp install: checksum mismatch for $asset (expected $expected, got $actual); refusing to install"
    }
    Write-Host "Checksum verified."

    Expand-Archive -Path (Join-Path $tmp $asset) -DestinationPath $tmp -Force
    $exe = Join-Path $tmp 'xlfp.exe'
    if (-not (Test-Path $exe)) { throw "xlfp install: archive did not contain xlfp.exe" }

    New-Item -ItemType Directory -Force -Path $installDir | Out-Null
    Copy-Item $exe (Join-Path $installDir 'xlfp.exe') -Force
    Write-Host "Installed: $(Join-Path $installDir 'xlfp.exe')"
    & (Join-Path $installDir 'xlfp.exe') version

    if (($env:Path -split ';') -notcontains $installDir) {
        Write-Host ""
        Write-Host "Note: $installDir is not on your PATH yet. To add it for your user account, run:"
        Write-Host "  [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path','User') + ';$installDir', 'User')"
        Write-Host "then open a new terminal."
    }

    Write-Host ""
    Write-Host "Next steps:"
    Write-Host "  `$env:XLFP_API_KEY = 'the key for the endpoint you want to check'"
    Write-Host "  xlfp            # guided mode, or: xlfp explain"
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
