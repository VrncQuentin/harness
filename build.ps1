[CmdletBinding()]
param(
    [string]$OutDir = "dist",
    [string]$Arch = "",
    [switch]$Test
)

$ErrorActionPreference = "Stop"

$_commit  = git rev-parse --short HEAD 2>&1
$Commit   = if ($LASTEXITCODE -eq 0) { "$_commit".Trim() } else { "unknown" }
$_version = & { $ErrorActionPreference = 'SilentlyContinue'; git describe --tags --exact-match 2>&1 }
$Version  = if ($LASTEXITCODE -eq 0) { "$_version".Trim() } else { "dev" }
$Built   = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ")

if ($Test) {
    Write-Host "==> Running tests..." -ForegroundColor Cyan
    go test ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

if ([string]::IsNullOrWhiteSpace($Arch)) {
    $Arch = (go env GOARCH).Trim()
}

Write-Host "==> Building harness ($Version @ $Commit, windows/$Arch)..." -ForegroundColor Cyan

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$LdFlags = "-s -w -H windowsgui " +
           "-X main.version=$Version " +
           "-X main.commit=$Commit " +
           "-X main.builtAt=$Built"

$Env:GOOS        = "windows"
$Env:GOARCH      = $Arch
$Env:CGO_ENABLED = "0"

go build -trimpath -ldflags $LdFlags -o "$OutDir\harness.exe" .\cmd\harness

if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

$Size = (Get-Item "$OutDir\harness.exe").Length / 1MB
Write-Host "==> $OutDir\harness.exe  ($([math]::Round($Size, 1)) MB)" -ForegroundColor Green
