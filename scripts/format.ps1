[CmdletBinding(DefaultParameterSetName = "All")]
param(
    [Parameter(ParameterSetName = "Files", Position = 0, ValueFromRemainingArguments)]
    [string[]]$Files,

    [Parameter(ParameterSetName = "Staged")]
    [switch]$Staged,

    [Parameter(ParameterSetName = "All")]
    [switch]$All,

    [switch]$Check
)

$ErrorActionPreference = "Stop"

$RepoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $RepoRoot

$CrlfExts = @(".ps1", ".bat", ".cmd")
$AsciiExts = @(".ps1", ".go", ".toml", ".yml", ".yaml", ".json", ".sh",
               ".bat", ".cmd", ".html", ".css", ".js", ".ts", ".gitattributes",
               ".gitignore", ".editorconfig", ".golangci.yml")
$TextExts = $AsciiExts + @(".md", ".txt")

$UnicodeMap = [ordered]@{
    [char]0x2010 = "-"
    [char]0x2011 = "-"
    [char]0x2012 = "-"
    [char]0x2013 = "-"
    [char]0x2014 = "-"
    [char]0x2015 = "-"
    [char]0x2018 = "'"
    [char]0x2019 = "'"
    [char]0x201A = "'"
    [char]0x201C = '"'
    [char]0x201D = '"'
    [char]0x201E = '"'
    [char]0x2026 = "..."
    [char]0x2022 = "*"
    [char]0x00A0 = " "
}

function Is-BinaryFile {
    param([string]$Path)
    $fs = [System.IO.File]::OpenRead($Path)
    try {
        $buf = New-Object byte[] ([Math]::Min(8192, $fs.Length))
        [void]$fs.Read($buf, 0, $buf.Length)
        foreach ($b in $buf) { if ($b -eq 0) { return $true } }
        return $false
    } finally { $fs.Dispose() }
}

function Get-FileExt {
    param([string]$Path)
    $leaf = Split-Path -Leaf $Path
    if ($leaf.StartsWith(".")) { return $leaf }
    return [System.IO.Path]::GetExtension($leaf).ToLower()
}

function Format-Content {
    param([string]$Path, [string]$Ext, [string]$Text)

    $changed = @()

    if ($AsciiExts -contains $Ext) {
        $sb = New-Object System.Text.StringBuilder
        $hadUnicode = $false
        foreach ($ch in $Text.ToCharArray()) {
            if ($UnicodeMap.Contains($ch)) {
                [void]$sb.Append($UnicodeMap[$ch])
                $hadUnicode = $true
            } else {
                [void]$sb.Append($ch)
            }
        }
        if ($hadUnicode) {
            $Text = $sb.ToString()
            $changed += "unicode"
        }
    }

    $wantCrlf = $CrlfExts -contains $Ext
    $normalized = $Text -replace "`r`n", "`n"
    $hasCr = $normalized -match "`r"
    if ($hasCr) { $normalized = $normalized -replace "`r", "`n" }

    if ($wantCrlf) {
        $finalText = $normalized -replace "`n", "`r`n"
    } else {
        $finalText = $normalized
    }
    if ($finalText -ne $Text) { $changed += "eol" }
    $Text = $finalText

    $nl = if ($wantCrlf) { "`r`n" } else { "`n" }
    if ($Text.Length -gt 0 -and -not $Text.EndsWith($nl)) {
        $Text = $Text.TrimEnd("`r", "`n") + $nl
        if ($changed -notcontains "eol") { $changed += "trailing-nl" }
    }

    return @{ Text = $Text; Changed = $changed }
}

function Read-FileText {
    param([string]$Path)
    $bytes = [System.IO.File]::ReadAllBytes($Path)
    $hasBom = $bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF
    $start = if ($hasBom) { 3 } else { 0 }
    $text = [System.Text.Encoding]::UTF8.GetString($bytes, $start, $bytes.Length - $start)
    return @{ Text = $text; HadBom = $hasBom }
}

function Write-FileText {
    param([string]$Path, [string]$Text, [bool]$WriteBom)
    $enc = New-Object System.Text.UTF8Encoding($WriteBom)
    [System.IO.File]::WriteAllText($Path, $Text, $enc)
}

function Format-File {
    param([string]$Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { return $null }

    $ext = Get-FileExt $Path
    if ($TextExts -notcontains $ext) { return $null }
    if (Is-BinaryFile $Path) { return $null }

    $read = Read-FileText $Path
    $wantBom = $ext -eq ".ps1"
    $result = Format-Content -Path $Path -Ext $ext -Text $read.Text

    $bomChanged = $read.HadBom -ne $wantBom
    if ($bomChanged) { $result.Changed += if ($wantBom) { "add-bom" } else { "strip-bom" } }

    if ($result.Changed.Count -eq 0) { return $null }

    if (-not $Check) {
        Write-FileText -Path $Path -Text $result.Text -WriteBom $wantBom
    }
    return @{ Path = $Path; Changed = $result.Changed }
}

function Get-StagedFiles {
    $out = & git diff --cached --name-only --diff-filter=ACMR 2>$null
    if ($LASTEXITCODE -ne 0) { throw "not a git repo" }
    return $out | Where-Object { $_ }
}

function Get-AllTrackedFiles {
    $out = & git ls-files -co --exclude-standard 2>$null
    if ($LASTEXITCODE -ne 0) { throw "not a git repo" }
    return $out | Where-Object { $_ } | Sort-Object -Unique
}

switch ($PSCmdlet.ParameterSetName) {
    "Files"  { $targets = $Files }
    "Staged" { $targets = Get-StagedFiles }
    "All"    { $targets = Get-AllTrackedFiles }
}

$modified = @()
foreach ($t in $targets) {
    $full = if ([System.IO.Path]::IsPathRooted($t)) { $t } else { Join-Path $RepoRoot $t }
    $r = Format-File -Path $full
    if ($r) { $modified += $r }
}

if ($modified.Count -eq 0) {
    if (-not $Check) { Write-Host "format: clean" -ForegroundColor Green }
    exit 0
}

$verb = if ($Check) { "would fix" } else { "fixed" }
Write-Host "format: $verb $($modified.Count) file(s)" -ForegroundColor Yellow
$rootPrefix = $RepoRoot.TrimEnd("\", "/") + [System.IO.Path]::DirectorySeparatorChar
foreach ($m in $modified) {
    $rel = if ($m.Path.StartsWith($rootPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        $m.Path.Substring($rootPrefix.Length)
    } else { $m.Path }
    Write-Host ("  {0,-40} [{1}]" -f $rel, ($m.Changed -join ","))
}

if ($Check) { exit 1 } else { exit 0 }
