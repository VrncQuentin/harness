[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$RepoRoot = Split-Path -Parent $PSScriptRoot

Push-Location $RepoRoot
try {
    git config core.hooksPath scripts/hooks
    if ($LASTEXITCODE -ne 0) { throw "git config failed" }
    Write-Host "hooks: core.hooksPath -> scripts/hooks" -ForegroundColor Green
} finally {
    Pop-Location
}
