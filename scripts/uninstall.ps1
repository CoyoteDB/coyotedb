<#
.SYNOPSIS
  CoyoteDB uninstaller for Windows (PowerShell 5.1 or later).

.DESCRIPTION
  Stops and removes the background service (and its firewall rule), removes
  %ProgramFiles%\CoyoteDB and takes it off the system PATH. Your data in
  %ProgramData%\CoyoteDB is kept unless you pass -Purge, which deletes the
  database for good and asks you to confirm first (unless -Yes).
  Asks for administrator rights (one Windows prompt) for the removal step.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File uninstall.ps1 -DryRun
#>
[CmdletBinding()]
param(
    [switch]$DryRun,
    [switch]$Purge,
    [switch]$Yes
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$InstallDir = Join-Path $env:ProgramFiles 'CoyoteDB'
$target = Join-Path $InstallDir 'coyote.exe'

function Fail([string]$Message) {
    Write-Host "Error: $Message" -ForegroundColor Red
    exit 1
}

function Say([string]$Message) { Write-Host $Message }

if ($Purge -and -not $Yes -and -not $DryRun) {
    Say '-Purge deletes this machine''s copy of the database for good.'
    $answer = Read-Host "If this is the only copy, the data is gone. Type 'delete' to continue"
    if ($answer -ne 'delete') { Fail 'not confirmed; nothing was changed' }
}

$purgeFlag = if ($Purge) { '1' } else { '0' }

$elevated = @'
param([string]$Dir, [string]$Exe, [string]$Purge, [string]$Pause = '0')
$ErrorActionPreference = 'Stop'
function Done([int]$Code) {
    if ($Code -ne 0 -and $Pause -eq '1') { Read-Host 'The uninstall step failed. Press Enter to close this window' | Out-Null }
    exit $Code
}
try {
    if (Test-Path -LiteralPath $Exe) {
        if ($Purge -eq '1') { & $Exe service uninstall --purge } else { & $Exe service uninstall }
        if ($LASTEXITCODE -ne 0) { Done $LASTEXITCODE }
    }
    else {
        Write-Host "$Exe not found; skipping the service step."
    }
    $path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    $parts = $path -split ';' | Where-Object { $_ -and ($_ -ne $Dir) }
    $new = $parts -join ';'
    if ($new -ne $path.TrimEnd(';')) {
        [Environment]::SetEnvironmentVariable('Path', $new, 'Machine')
        Write-Host "Removed $Dir from the system PATH"
    }
    if (Test-Path -LiteralPath $Dir) {
        Remove-Item -LiteralPath $Dir -Recurse -Force
        Write-Host "Removed $Dir"
    }
    Done 0
}
catch {
    Write-Host "Error: $($_.Exception.Message)" -ForegroundColor Red
    Done 1
}
'@

Say "Administrator rights are needed to stop and remove the background service and to remove $InstallDir."
if ($DryRun) {
    Say '  would ask Windows for administrator rights, then:'
    if ($Purge) { Say "  would run: `"$target`" service uninstall --purge" } else { Say "  would run: `"$target`" service uninstall" }
    Say "  would remove $InstallDir from the system PATH and delete it"
    Say ''
    Say 'Dry run finished; nothing was changed.'
    exit 0
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("coyote-uninstall-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $helper = Join-Path $tmp 'elevated-uninstall.ps1'
    Set-Content -LiteralPath $helper -Value $elevated -Encoding UTF8
    foreach ($v in @($helper, $InstallDir, $target)) {
        if ($v.Contains('"')) { Fail "unexpected quote in path: $v" }
    }
    $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
    if ($isAdmin) {
        & powershell.exe @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $helper, '-Dir', $InstallDir, '-Exe', $target, '-Purge', $purgeFlag)
        $code = $LASTEXITCODE
    }
    else {
        Say 'Windows will now ask for permission.'
        $argList = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$helper`"",
            '-Dir', "`"$InstallDir`"", '-Exe', "`"$target`"", '-Purge', $purgeFlag, '-Pause', '1')
        try {
            $p = Start-Process -FilePath 'powershell.exe' -ArgumentList $argList -Verb RunAs -Wait -PassThru
        }
        catch {
            Fail 'administrator rights were not granted; nothing was changed.'
        }
        $code = $p.ExitCode
    }
    if ($code -ne 0) { Fail "the uninstall step failed (exit code $code)." }
    Say ''
    if ($Purge) { Say 'Done. CoyoteDB and its data were removed from this machine.' }
    else { Say 'Done. CoyoteDB was removed; your data was kept (see the message above for where).' }
}
finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
