<#
.SYNOPSIS
    Build BLAT test app and produce NSIS installer.

.DESCRIPTION
    Pipeline:
      1. fyne package (go build + embed icon + write Windows manifest) -> bin\blat.exe
      2. Mirror confs\ to dist\stage\confs\ (for NSIS packaging)
      3. Run makensis -> dist\BLAT-Setup-<VERSION>.exe

.PARAMETER Version
    Installer version. Defaults to `git describe`, falls back to 0.0.0-dev.

.PARAMETER AppId
    Reverse-domain app id passed to fyne package. Default: com.poersmart.blat.

.PARAMETER SkipInstaller
    Only build the Go binary; skip NSIS packaging.

.PARAMETER SkipBuild
    Only run NSIS packaging (assume bin\blat.exe already exists).

.PARAMETER Toolchain
    MSYS2 toolchain flavor to use for cgo linking. Default: ucrt64.
    One of: ucrt64, mingw64, mingw32, clang64, clangarm64.

.PARAMETER SkipMsys2
    Skip MSYS2 toolchain activation. Use this if you build with CGO_ENABLED=0
    or if your default gcc already works (rare on Windows).

.EXAMPLE
    pwsh scripts\build.ps1
    pwsh scripts\build.ps1 -Version 1.2.3
    pwsh scripts\build.ps1 -SkipBuild
    pwsh scripts\build.ps1 -SkipMsys2
    pwsh scripts\build.ps1 -AppId com.poersmart.blat -Toolchain ucrt64
#>
[CmdletBinding()]
param(
    [string]$Version,
    [string]$AppId = 'com.poersmart.blat',
    [switch]$SkipInstaller,
    [switch]$SkipBuild,
    [ValidateSet('ucrt64', 'mingw64', 'mingw32', 'clang64', 'clangarm64')]
    [string]$Toolchain = 'ucrt64',
    [switch]$SkipMsys2
)

$ErrorActionPreference = "Stop"

# -------- Toolchain: prefer MSYS2 over Strawberry's broken mingw for cgo --------
if (-not $SkipMsys2) {
    . (Join-Path $PSScriptRoot 'use-msys2.ps1')
    Set-Msys2Toolchain -Toolchain $Toolchain
    Write-Host "[build] Using MSYS2 toolchain: $Toolchain" -ForegroundColor DarkGray
} else {
    Write-Host "[build] Skipping MSYS2 toolchain (using system default gcc)" -ForegroundColor DarkYellow
}

# -------- Paths (resolve absolute to avoid CWD issues) --------
$ScriptDir   = $PSScriptRoot
$ProjectRoot = (Resolve-Path (Join-Path $ScriptDir "..")).Path
$AppIco      = Join-Path $ProjectRoot "app.ico"
$BinDir      = Join-Path $ProjectRoot "bin"
$ExePath     = Join-Path $BinDir "blat.exe"
$ConfsDir    = Join-Path $ProjectRoot "confs"
$StageDir    = Join-Path $ProjectRoot "dist\stage"
$NsiPath     = Join-Path $ScriptDir "installer.nsi"
$DistDir     = Join-Path $ProjectRoot "dist"

Write-Host "[build] Project: $ProjectRoot"

# -------- Version --------
if (-not $Version) {
    $gitDesc = (& git -C $ProjectRoot describe --tags --always --dirty=-dev 2>$null)
    if ($LASTEXITCODE -eq 0 -and $gitDesc) {
        $Version = $gitDesc -replace '^v', ''
    } else {
        $Version = "0.0.0-dev"
    }
}
Write-Host "[build] Version: $Version"

# -------- 1. fyne package (go build + embed icon + Windows manifest) --------
if (-not $SkipBuild) {
    if (-not (Test-Path $AppIco)) {
        throw "app.ico not found: $AppIco. Place a Windows .ico at the project root."
    }
    if (-not (Test-Path $BinDir)) {
        New-Item -ItemType Directory -Path $BinDir | Out-Null
    }

    $fyneSrcDir = Join-Path $ProjectRoot 'cmd\blat'
    $fyneExeOut = Join-Path $fyneSrcDir 'blat.exe'
    if (Test-Path $fyneExeOut) { Remove-Item $fyneExeOut -Force }

    Write-Host "[build] fyne package --icon $AppIco --app-id $AppId --release"
    Push-Location $fyneSrcDir
    try {
        & fyne package --src . --icon (Resolve-Path $AppIco).Path --app-id $AppId --release
        if ($LASTEXITCODE -ne 0) { throw "fyne package failed (exit $LASTEXITCODE)" }
    } finally {
        Pop-Location
    }

    if (-not (Test-Path $fyneExeOut)) {
        throw "fyne package did not produce $fyneExeOut"
    }
    Copy-Item -Path $fyneExeOut -Destination $ExePath -Force
    Write-Host "[build] Copied -> $ExePath"
}

if (-not (Test-Path $ExePath)) {
    throw "exe not found at $ExePath; drop -SkipBuild to compile first"
}

# -------- 2. NSIS --------
if (-not $SkipInstaller) {
    Write-Host "[build] Cleaning old installers in $DistDir"
    if (Test-Path $DistDir) {
        Get-ChildItem -Path $DistDir -Filter "*.exe" -ErrorAction SilentlyContinue | Remove-Item -Force
    }
    Write-Host "[build] Preparing NSIS stage dir"
    $stageConfs = Join-Path $StageDir "confs"
    if (Test-Path $StageDir) {
        Remove-Item -Recurse -Force $StageDir
    }
    New-Item -ItemType Directory -Path $stageConfs -Force | Out-Null

    # Mirror confs\* to stage\confs\
    Get-ChildItem -Path $ConfsDir -Force | ForEach-Object {
        $target = Join-Path $stageConfs $_.Name
        if ($_.PSIsContainer) {
            Copy-Item -Path $_.FullName -Destination $target -Recurse -Force
        } else {
            Copy-Item -Path $_.FullName -Destination $target -Force
        }
    }

    Write-Host "[build] Running makensis"
    $makensis = $null
    $candidates = @(
        (Join-Path $env:ProgramFiles "NSIS\makensis.exe"),
        (Join-Path ${env:ProgramFiles(x86)} "NSIS\makensis.exe"),
        (Get-Command makensis.exe -ErrorAction SilentlyContinue).Source
    )
    foreach ($p in $candidates) {
        if ($p -and (Test-Path $p)) { $makensis = $p; break }
    }
    if (-not $makensis) {
        throw "makensis.exe not found. Install NSIS 3.x."
    }
    Write-Host "[build] Using makensis: $makensis"

    if (-not (Test-Path $DistDir)) {
        New-Item -ItemType Directory -Path $DistDir -Force | Out-Null
    }

    # /INPUTCHARSET and /DVERSION are passed as separate tokens (NSIS docs only
    # accept key=value form for /D, but not for /INPUTCHARSET).
    # Quoted strings with leading slash prevent PowerShell from mis-parsing.
    # /INPUTCHARSET UTF8 lets makensis read the .nsi (containing Chinese strings
    # like "BLAT 测试程序") as UTF-8 instead of the legacy ANSI codepage.
    & $makensis "/DVERSION=$Version" /INPUTCHARSET UTF8 $NsiPath
    if ($LASTEXITCODE -ne 0) { throw "NSIS packaging failed (exit $LASTEXITCODE)" }

    $installer = Join-Path $DistDir "BLAT-Setup-$Version.exe"
    if (Test-Path $installer) {
        $sizeMb = "{0:N2}" -f ((Get-Item $installer).Length / 1MB)
        Write-Host ""
        Write-Host "[build] Installer: $installer" -ForegroundColor Green
        Write-Host "[build] Size: $sizeMb MB"
    }
}

Write-Host "[build] Done" -ForegroundColor Green
