<#
.SYNOPSIS
    Activate the MSYS2 toolchain for cgo/Go builds in the current PowerShell session.

.DESCRIPTION
    On Windows, Go's cgo linker defaults to whichever gcc is found first on PATH.
    Strawberry Perl ships an older mingw-w64 (gcc 8.3.0) that is missing libssp
    (Stack Smashing Protector), causing cgo links to fail with:

        undefined reference to `__stack_chk_fail'
        undefined reference to `__stack_chk_guard'

    This script defines Set-Msys2Toolchain which:
      1. Locates the MSYS2 installation (default C:\msys64).
      2. Sets CC and CXX to the chosen toolchain's gcc/g++.
      3. Prepends the toolchain's bin dir to PATH and removes any Strawberry
         `c\bin` entry to prevent mixed toolchain use.

    Three usage modes:

    (a) Direct call: switches env, prints diagnostics. Closing the window
        reverts the changes.

        pwsh .\scripts\use-msys2.ps1

    (b) Direct call with a command to run after switching:

        pwsh .\scripts\use-msys2.ps1 fyne package --src .\cmd\blat ...

    (c) Dot-source from another script to get just the function (no side effects):

        . .\scripts\use-msys2.ps1
        Set-Msys2Toolchain -Toolchain ucrt64

.PARAMETER Msys2Root
    MSYS2 installation directory. Defaults to C:\msys64.

.PARAMETER Toolchain
    One of: ucrt64 (default, recommended), mingw64, mingw32, clang64, clangarm64.

.PARAMETER Command
    Optional positional argument: a command (and its arguments) to run after the
    toolchain is activated. The command inherits the updated environment.

.EXAMPLE
    . .\scripts\use-msys2.ps1
    fyne package --src .\cmd\blat --icon Icon.png --app-id com.poersmart.blat --release

.EXAMPLE
    pwsh .\scripts\use-msys2.ps1 fyne package --src .\cmd\blat --icon Icon.png --app-id com.poersmart.blat --release

.EXAMPLE
    pwsh .\scripts\use-msys2.ps1 -Toolchain mingw64 go build -ldflags "-s -w" -o bin\blat.exe .\cmd\blat
#>
[CmdletBinding()]
param(
    [string]$Msys2Root = 'C:\msys64',
    [ValidateSet('ucrt64', 'mingw64', 'mingw32', 'clang64', 'clangarm64')]
    [string]$Toolchain = 'ucrt64',
    [Parameter(Position = 0)]
    [string]$Command,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$CommandArgs
)

function Set-Msys2Toolchain {
    <#
    .SYNOPSIS
        Switch CC/CXX/PATH to the selected MSYS2 toolchain for the current session.
    .PARAMETER Msys2Root
        MSYS2 installation directory (default C:\msys64).
    .PARAMETER Toolchain
        ucrt64 (default) | mingw64 | mingw32 | clang64 | clangarm64.
    #>
    [CmdletBinding()]
    param(
        [string]$Msys2Root = 'C:\msys64',
        [ValidateSet('ucrt64', 'mingw64', 'mingw32', 'clang64', 'clangarm64')]
        [string]$Toolchain = 'ucrt64'
    )

    $binDir = Join-Path $Msys2Root "$Toolchain\bin"
    $gccExe = Join-Path $binDir "gcc.exe"
    $gxxExe = Join-Path $binDir "g++.exe"
    $ldExe  = Join-Path $binDir "ld.exe"

    if (-not (Test-Path -LiteralPath $Msys2Root)) {
        throw "MSYS2 not found at '$Msys2Root'. Pass -Msys2Root <path> if installed elsewhere."
    }
    if (-not (Test-Path -LiteralPath $gccExe)) {
        throw "gcc.exe not found at '$gccExe'. Has the '$Toolchain' toolchain been installed? Run in MSYS2: pacman -S mingw-w64-ucrt-x86_64-gcc"
    }
    if (-not (Test-Path -LiteralPath $ldExe)) {
        throw "ld.exe not found at '$ldExe'. The toolchain appears incomplete."
    }

    $strawberryDirs = @(
        'C:\Strawberry\c\bin',
        'C:\Strawberry\c\x86_64-w64-mingw32\bin'
    )
    $cleanedPath = ($env:Path -split ';' |
        Where-Object { $_ -and ($strawberryDirs -notcontains $_) }) -join ';'

    # Prepend msys2 bin, de-duplicate.
    $segments = @($binDir) + ($cleanedPath -split ';') | Where-Object { $_ }
    $env:Path = ($segments | Select-Object -Unique) -join ';'

    $env:CC  = $gccExe
    $env:CXX = $gxxExe

    $gccLine = (& $gccExe --version | Select-Object -First 1)
    Write-Host "[msys2] Toolchain: $Toolchain" -ForegroundColor Cyan
    Write-Host "[msys2] gcc:      $gccLine"
    Write-Host "[msys2] CC:       $($env:CC)"
    Write-Host "[msys2] PATH[0]:  $((($env:Path -split ';')[0]))"
}

# -------- Auto-activate only when run directly (not when dot-sourced) --------
if ($MyInvocation.InvocationName -ne '.') {
    Set-Msys2Toolchain -Msys2Root $Msys2Root -Toolchain $Toolchain

    if ($Command) {
        if (-not $CommandArgs) { $CommandArgs = @() }
        Write-Host "[msys2] Running: $Command $($CommandArgs -join ' ')" -ForegroundColor Green
        & $Command @CommandArgs
        exit $LASTEXITCODE
    }
}
