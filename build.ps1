#Requires -Version 7.0
[CmdletBinding()]
param(
    [string]$OutDir = "dist",
    [switch]$Test
)

$ErrorActionPreference = "Stop"

$Module  = (go env GOMODULE 2>$null)
$Commit  = (git rev-parse --short HEAD 2>$null) ?? "unknown"
$Version = (git describe --tags --exact-match 2>$null) ?? "dev"
$Built   = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")

if ($Test) {
    Write-Host "==> Running tests..." -ForegroundColor Cyan
    go test ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

Write-Host "==> Building harness ($Version @ $Commit)..." -ForegroundColor Cyan

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$LdFlags = "-s -w " +
           "-X main.version=$Version " +
           "-X main.commit=$Commit " +
           "-X main.builtAt=$Built"

$Env:GOOS   = "windows"
$Env:GOARCH = "amd64"
$Env:CGO_ENABLED = "0"

go build -trimpath -ldflags $LdFlags -o "$OutDir\harness.exe" .\cmd\harness

if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

$Size = (Get-Item "$OutDir\harness.exe").Length / 1MB
Write-Host "==> $OutDir\harness.exe  ($([math]::Round($Size, 1)) MB)" -ForegroundColor Green
