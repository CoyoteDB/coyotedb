<#
.SYNOPSIS
  CoyoteDB installer for Windows (PowerShell 5.1 or later).

.DESCRIPTION
  1. Works out this machine's processor (amd64 or arm64).
  2. Downloads <base>\coyote-windows-<arch>.exe and <base>\SHA256SUMS.
  3. Checks the program's sha256 against SHA256SUMS and stops if it differs.
  4. Asks for administrator rights (one Windows prompt) only for the install
     step: copy to %ProgramFiles%\CoyoteDB, add that folder to the system
     PATH, and run "coyote service install" (the background service).

  There is no public release yet, so nothing is downloaded from a built-in
  address: set COYOTE_RELEASE_BASE (or pass -Base) to the folder that holds
  the release files (https:// or, for testing, file://).

.EXAMPLE
  $env:COYOTE_RELEASE_BASE = 'https://<release host>/<version>'
  powershell -ExecutionPolicy Bypass -File install.ps1 -DryRun
#>
[CmdletBinding()]
param(
    [switch]$DryRun,
    [switch]$NoService,
    [string]$Base = $env:COYOTE_RELEASE_BASE
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Program = 'coyote'
$InstallDir = Join-Path $env:ProgramFiles 'CoyoteDB'
$NextStep = "Next: run 'coyote up' to start a new cluster on this machine, or 'coyote join <invite code>' to join one you already have (open a new PowerShell window first so PATH is updated)."

function Fail([string]$Message) {
    Write-Host "Error: $Message" -ForegroundColor Red
    exit 1
}

function Say([string]$Message) { Write-Host $Message }

# ---- what to download -------------------------------------------------------
if ([string]::IsNullOrWhiteSpace($Base)) {
    Fail ("COYOTE_RELEASE_BASE is not set.`n" +
        "There is no public CoyoteDB release yet, so this script has no download address built in.`n" +
        "Set it to the folder that holds the release files, for example:`n" +
        "    `$env:COYOTE_RELEASE_BASE = 'https://example.invalid/coyote/1.0.0'")
}
if (-not ($Base.StartsWith('https://') -or $Base.StartsWith('file://'))) {
    Fail "COYOTE_RELEASE_BASE must start with https:// (got: $Base)"
}
$Base = $Base.TrimEnd('/')

$cpu = $env:PROCESSOR_ARCHITEW6432
if (-not $cpu) { $cpu = $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Fail "unsupported processor: $cpu (CoyoteDB runs on 64-bit Intel/AMD and ARM)" }
}
$asset = "$Program-windows-$arch.exe"
$target = Join-Path $InstallDir "$Program.exe"

Say "Installing CoyoteDB for windows/$arch"
Say "  from: $Base/$asset"
Say "  to:   $target"
if ($DryRun) { Say '  (dry run: nothing will be changed)' }

function Get-Release([string]$Url, [string]$OutFile) {
    if ($Url.StartsWith('file://')) {
        Copy-Item -LiteralPath ([Uri]$Url).LocalPath -Destination $OutFile -Force
        return
    }
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing
}

# ---- download + verify ------------------------------------------------------
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("coyote-install-" + [Guid]::NewGuid().ToString('N'))
try {
    if ($DryRun) {
        Say "  would download $Base/$asset and $Base/SHA256SUMS"
        Say "  would check the sha256 of $asset against SHA256SUMS and stop on a mismatch"
    }
    else {
        New-Item -ItemType Directory -Path $tmp | Out-Null
        $sums = Join-Path $tmp 'SHA256SUMS'
        $exe = Join-Path $tmp $asset
        try { Get-Release "$Base/SHA256SUMS" $sums } catch { Fail "could not download $Base/SHA256SUMS ($($_.Exception.Message))" }
        try { Get-Release "$Base/$asset" $exe } catch { Fail "could not download $Base/$asset ($($_.Exception.Message))" }

        $want = $null
        foreach ($line in Get-Content -LiteralPath $sums) {
            if ($line -match '^([0-9a-fA-F]{64}) [ *](.+)$' -and $Matches[2].Trim() -eq $asset) {
                $want = $Matches[1].ToLowerInvariant()
                break
            }
        }
        if (-not $want) { Fail "SHA256SUMS has no valid entry for $asset; refusing to install" }
        $got = (Get-FileHash -LiteralPath $exe -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($got -ne $want) {
            Fail ("the downloaded program does not match its checksum; refusing to install.`n" +
                "  expected: $want`n  got:      $got`n" +
                "The download may be damaged or tampered with. Nothing was installed.")
        }
        Say "Checksum OK ($want)"
    }

    # ---- install (needs administrator rights) -------------------------------
    # The elevated part is a small script with its inputs as parameters, run
    # in a new elevated PowerShell (one UAC prompt).
    $elevated = @'
param([string]$Source, [string]$Dir, [string]$Exe, [string]$Service, [string]$Pause = '0')
$ErrorActionPreference = 'Stop'
function Done([int]$Code) {
    if ($Code -ne 0 -and $Pause -eq '1') { Read-Host 'The install step failed. Press Enter to close this window' | Out-Null }
    exit $Code
}
try {
    $wasRunning = $false
    if (Test-Path -LiteralPath $Exe) {
        & $Exe service status *> $null
        if ($LASTEXITCODE -eq 0) {
            $wasRunning = $true
            Write-Host 'Stopping the running CoyoteDB service for the upgrade...'
            & $Exe service stop
            if ($LASTEXITCODE -ne 0) { Done $LASTEXITCODE }
        }
    }
    New-Item -ItemType Directory -Force -Path $Dir | Out-Null
    Copy-Item -LiteralPath $Source -Destination $Exe -Force
    Write-Host "Installed $Exe"
    $path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (-not (($path -split ';') -contains $Dir)) {
        [Environment]::SetEnvironmentVariable('Path', ($path.TrimEnd(';') + ';' + $Dir), 'Machine')
        Write-Host "Added $Dir to the system PATH"
    }
    if ($Service -eq '1') {
        & $Exe service install
        if ($LASTEXITCODE -ne 0) { Done $LASTEXITCODE }
    }
    if ($wasRunning) {
        & $Exe service start
        if ($LASTEXITCODE -ne 0) { Done $LASTEXITCODE }
    }
    Done 0
}
catch {
    Write-Host "Error: $($_.Exception.Message)" -ForegroundColor Red
    Done 1
}
'@
    $serviceFlag = if ($NoService) { '0' } else { '1' }

    Say ''
    if ($NoService) {
        Say "Administrator rights are needed to copy coyote into $InstallDir."
    }
    else {
        Say "Administrator rights are needed to copy coyote into $InstallDir and to set it up as a"
        Say 'background service (so it keeps running after you sign out and starts with Windows).'
    }

    if ($DryRun) {
        Say "  would ask Windows for administrator rights, then:"
        Say "  would copy the program to $target and add $InstallDir to the system PATH"
        if (-not $NoService) { Say "  would run: `"$target`" service install" }
        Say ''
        Say 'Dry run finished; nothing was changed.'
        exit 0
    }

    $helper = Join-Path $tmp 'elevated-install.ps1'
    Set-Content -LiteralPath $helper -Value $elevated -Encoding UTF8

    # Start-Process joins ArgumentList with spaces and does not quote, so
    # each value is quoted here; none of them can contain a quote.
    foreach ($v in @($helper, $exe, $InstallDir, $target)) {
        if ($v.Contains('"')) { Fail "unexpected quote in path: $v" }
    }
    $argList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$helper`"",
        '-Source', "`"$exe`"", '-Dir', "`"$InstallDir`"", '-Exe', "`"$target`"", '-Service', $serviceFlag, '-Pause', '1')

    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if ($isAdmin) {
        & powershell.exe @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $helper,
            '-Source', $exe, '-Dir', $InstallDir, '-Exe', $target, '-Service', $serviceFlag)
        $code = $LASTEXITCODE
    }
    else {
        Say 'Windows will now ask for permission.'
        try {
            $p = Start-Process -FilePath 'powershell.exe' -ArgumentList $argList -Verb RunAs -Wait -PassThru
        }
        catch {
            Fail 'administrator rights were not granted; nothing was installed.'
        }
        $code = $p.ExitCode
    }
    if ($code -ne 0) { Fail "the install step failed (exit code $code)." }

    Say ''
    Say 'Done. CoyoteDB is installed.'
    Say $NextStep
}
finally {
    if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue }
}
