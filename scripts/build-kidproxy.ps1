# Cross-compile kidproxy for the Pi (OpenWrt 25.x, aarch64) from Windows.
#
#   powershell -File scripts\build-kidproxy.ps1
#
# Produces proxy\dist\kidproxy. CGO is off: the SQLite driver is pure Go, so the
# binary is static and needs no runtime packages on the router.

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$out = Join-Path $root 'proxy\dist'

$go = (Get-Command go -ErrorAction SilentlyContinue).Source
if (-not $go) {
    $local = Join-Path $env:LOCALAPPDATA 'go-toolchain\go\bin\go.exe'
    if (Test-Path $local) { $go = $local } else { throw 'Go toolchain not found (install Go or set PATH).' }
}

New-Item -ItemType Directory -Force -Path $out | Out-Null
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'arm64'
$env:GOFLAGS = '-trimpath'

Push-Location (Join-Path $root 'proxy')
try {
    & $go build -ldflags '-s -w' -o (Join-Path $out 'kidproxy') .
    if ($LASTEXITCODE -ne 0) { throw "build failed ($LASTEXITCODE)" }
} finally {
    Pop-Location
}

$f = Get-Item (Join-Path $out 'kidproxy')
'{0}  {1:N1} MB' -f $f.FullName, ($f.Length / 1MB)
