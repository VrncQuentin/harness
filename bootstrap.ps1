[CmdletBinding()]
param(
    [ValidateSet("auto", "cuda", "rocm", "cpu")]
    [string]$Backend = "auto",
    [string]$QwenQuant = "UD-Q4_K_M",
    [switch]$NoPrompt,
    [switch]$Force
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

$RepoRoot  = $PSScriptRoot
$ModelsDir = Join-Path $RepoRoot "models"
$LlamaDir  = Join-Path $RepoRoot "llama.cpp"

$QwenRepo  = "unsloth/Qwen3.6-35B-A3B-GGUF"
$NomicRepo = "ggml-org/Nomic-Embed-Text-V2-GGUF"
$NomicFile = "nomic-embed-text-v2-moe-q8_0.gguf"

function Write-Step($msg) { Write-Host "==> $msg" -ForegroundColor Cyan }
function Write-Info($msg) { Write-Host "    $msg" -ForegroundColor Gray }
function Write-Warn($msg) { Write-Host "!!  $msg" -ForegroundColor Yellow }
function Write-OK($msg)   { Write-Host "==> $msg" -ForegroundColor Green }

function Confirm-Yes($prompt) {
    if ($NoPrompt) { return $false }
    $ans = Read-Host "$prompt (y/N)"
    return $ans -match '^[Yy]'
}

function Invoke-Download {
    param([Parameter(Mandatory)][string]$Url, [Parameter(Mandatory)][string]$Dest)

    $curl = Get-Command curl.exe -ErrorAction SilentlyContinue
    if (-not $curl) { throw "curl.exe not found on PATH (requires Windows 10 1803+)." }

    $tmp = "$Dest.part"
    & curl.exe -L --fail --retry 3 --retry-delay 2 --progress-bar -o $tmp $Url
    if ($LASTEXITCODE -ne 0) {
        if (Test-Path $tmp) { Remove-Item $tmp -Force }
        throw "Download failed: $Url"
    }
    Move-Item -Force $tmp $Dest
}

function Get-NvidiaCudaVersion {
    if (-not (Get-Command nvidia-smi.exe -ErrorAction SilentlyContinue)) { return $null }
    $out = & nvidia-smi.exe 2>$null
    if ($LASTEXITCODE -ne 0) { return $null }
    $m = $out | Select-String -Pattern 'CUDA Version:\s+(\d+\.\d+)' | Select-Object -First 1
    if (-not $m) { return $null }
    return [version]$m.Matches[0].Groups[1].Value
}

function Resolve-Backend {
    if ($Backend -ne "auto") {
        $info = @{ Name = $Backend }
        if ($Backend -eq "cuda") { $info.CudaVersion = (Get-NvidiaCudaVersion) }
        return $info
    }
    $cuda = Get-NvidiaCudaVersion
    if ($cuda) { return @{ Name = "cuda"; CudaVersion = $cuda } }

    if (Get-Command rocminfo.exe -ErrorAction SilentlyContinue) { return @{ Name = "rocm" } }

    $amd = Get-PnpDevice -Class Display -ErrorAction SilentlyContinue |
        Where-Object { $_.Manufacturer -match "Advanced Micro Devices" -or $_.FriendlyName -match "Radeon" }
    if ($amd) {
        Write-Warn "AMD GPU detected ($($amd[0].FriendlyName)) but rocminfo not on PATH."
        Write-Warn "Install ROCm or rerun with -Backend rocm to force."
    }
    return @{ Name = "cpu" }
}

function Get-LlamaRelease {
    Invoke-RestMethod -Uri "https://api.github.com/repos/ggml-org/llama.cpp/releases/latest" `
        -Headers @{ "User-Agent" = "harness-bootstrap"; "Accept" = "application/vnd.github+json" }
}

function Select-LlamaAssets {
    param($Release, $BackendInfo)

    $tag = $Release.tag_name
    $prefix = "llama-$tag-bin-win"

    switch ($BackendInfo.Name) {
        "cpu"  { return ,"$prefix-cpu-x64.zip" }
        "rocm" { return ,"$prefix-hip-radeon-x64.zip" }
        "cuda" {
            $flavors = foreach ($a in $Release.assets) {
                if ($a.name -match "^$prefix-cuda-(\d+\.\d+)-x64\.zip$") {
                    [pscustomobject]@{ Version = [version]$Matches[1]; Asset = $a.name }
                }
            }
            $flavors = $flavors | Sort-Object Version -Descending
            if (-not $flavors) { throw "No CUDA builds found in release $tag." }

            $driverCuda = $BackendInfo.CudaVersion
            if (-not $driverCuda) {
                Write-Warn "CUDA backend requested but nvidia-smi not available."
                Write-Warn "Picking latest CUDA build; driver must support CUDA $($flavors[0].Version)."
                $pick = $flavors[0]
            } else {
                $pick = $flavors | Where-Object { $_.Version -le $driverCuda } | Select-Object -First 1
                if (-not $pick) {
                    $min = ($flavors | Sort-Object Version | Select-Object -First 1).Version
                    Write-Warn "Driver supports CUDA $driverCuda; minimum required is $min."
                    if (Confirm-Yes "Open NVIDIA driver download page?") {
                        Start-Process "https://www.nvidia.com/Download/index.aspx"
                    }
                    throw "Driver too old - update NVIDIA driver and rerun."
                }
                Write-Info "Driver CUDA $driverCuda -> using cuda-$($pick.Version) build."
            }
            return @(
                "$prefix-cuda-$($pick.Version)-x64.zip",
                "cudart-llama-bin-win-cuda-$($pick.Version)-x64.zip"
            )
        }
    }
}

function Install-LlamaCpp {
    param($Release, [string[]]$AssetNames)

    if ((Test-Path (Join-Path $LlamaDir "llama-server.exe")) -and -not $Force) {
        Write-Info "llama.cpp already installed at $LlamaDir (use -Force to reinstall)"
        return
    }

    New-Item -ItemType Directory -Path $LlamaDir -Force | Out-Null

    foreach ($name in $AssetNames) {
        $asset = $Release.assets | Where-Object { $_.name -eq $name } | Select-Object -First 1
        if (-not $asset) { throw "Release $($Release.tag_name) has no asset named $name" }

        $zip = Join-Path $env:TEMP $name
        Write-Info "Downloading $name ($([math]::Round($asset.size / 1MB, 1)) MB)"
        Invoke-Download -Url $asset.browser_download_url -Dest $zip
        Write-Info "Extracting $name"
        Expand-Archive -Path $zip -DestinationPath $LlamaDir -Force
        Remove-Item $zip -Force
    }
    Write-OK "llama.cpp installed at $LlamaDir"
}

function Get-HFModel {
    param([string]$Repo, [string]$File, [string]$Dest)

    if ((Test-Path $Dest) -and -not $Force) {
        $gb = (Get-Item $Dest).Length / 1GB
        Write-Info "Already present: $(Split-Path -Leaf $Dest) ($([math]::Round($gb, 2)) GB)"
        return
    }
    $url = "https://huggingface.co/$Repo/resolve/main/$File"
    Write-Info "Downloading $File from $Repo"
    Invoke-Download -Url $url -Dest $Dest
}

Write-Step "Detecting backend..."
$backendInfo = Resolve-Backend
Write-Info "Backend: $($backendInfo.Name)"
if ($backendInfo.CudaVersion) { Write-Info "CUDA (driver max): $($backendInfo.CudaVersion)" }

Write-Step "Fetching latest llama.cpp release..."
$release = Get-LlamaRelease
Write-Info "Tag: $($release.tag_name)"

$assets = Select-LlamaAssets -Release $release -BackendInfo $backendInfo
foreach ($a in $assets) { Write-Info "Asset: $a" }

Write-Step "Installing llama.cpp..."
Install-LlamaCpp -Release $release -AssetNames $assets

New-Item -ItemType Directory -Path $ModelsDir -Force | Out-Null

Write-Step "Downloading Nomic Embed Text v2..."
Get-HFModel -Repo $NomicRepo -File $NomicFile -Dest (Join-Path $ModelsDir $NomicFile)

Write-Step "Downloading Qwen3.6-35B-A3B ($QwenQuant)..."
$qwenFile = "Qwen3.6-35B-A3B-$QwenQuant.gguf"
Get-HFModel -Repo $QwenRepo -File $qwenFile -Dest (Join-Path $ModelsDir $qwenFile)

Write-OK "Done."
Write-Info "llama-server: $(Join-Path $LlamaDir 'llama-server.exe')"
Write-Info "Qwen model:   $(Join-Path $ModelsDir $qwenFile)"
Write-Info "Nomic model:  $(Join-Path $ModelsDir $NomicFile)"
