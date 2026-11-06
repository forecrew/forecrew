# Starts forecrew-worker on a Windows Forecrew machine from the settings its
# provider delivered, as forecrew-start does on Linux. The settings are KEY=value
# lines in C:\ProgramData\Forecrew\run\bootstrap.env (see forecrew-start for the
# keys). The file is read as data, deleted once read, and the machine powers off
# when the worker ends: it is thrown away after its jobs. On Windows the folder
# is on the machine's own disk, not in RAM: only SYSTEM and administrators may
# read it (the ACL set here and at image build), the provider delivers the file
# into the running machine (Hyper-V PowerShell Direct; the image turns WinRM off)
# once it is assigned,
# never in the image or on a host share, and the disk is destroyed with the
# machine.
$ErrorActionPreference = 'Stop'

$root = if ($env:FORECREW_ROOT) { $env:FORECREW_ROOT } else { 'C:\ProgramData\Forecrew' }
$envFile = Join-Path $root 'run\bootstrap.env'
$config = Join-Path $root 'run\worker.yaml'
$worker = if ($env:FORECREW_WORKER) { $env:FORECREW_WORKER } else { Join-Path $root 'forecrew-worker.exe' }
$workDir = if ($env:FORECREW_WORK_DIR) { $env:FORECREW_WORK_DIR } else { 'C:\ProgramData\Forecrew\work' }

function Stop-Machine {
    if ($env:FORECREW_NO_SHUTDOWN) { return }
    Stop-Computer -Force
}

# Only SYSTEM and administrators read the folder the token arrives in.
if (Get-Command icacls -ErrorAction SilentlyContinue) {
    New-Item -ItemType Directory -Force -Path (Split-Path $envFile) | Out-Null
    icacls (Split-Path $envFile) /inheritance:r /grant:r 'SYSTEM:(OI)(CI)F' 'Administrators:(OI)(CI)F' | Out-Null
}

# The provider delivers the file around boot: wait for it for a while.
$deadline = (Get-Date).AddMinutes(10)
while (-not (Test-Path $envFile) -and (Get-Date) -lt $deadline) { Start-Sleep -Seconds 2 }
if (-not (Test-Path $envFile)) {
    Write-Error "forecrew-start: $envFile is missing: the provider did not deliver the bootstrap settings"
    Stop-Machine
    exit 1
}

$allowed = @('FORECREW_SERVER_URL', 'FORECREW_BOOTSTRAP_TOKEN', 'FORECREW_WORKER_TOKEN', 'FORECREW_WORKER_NAME', 'FORECREW_WORKER_LABELS', 'FORECREW_WORKER_MAX_JOBS')
$s = @{}
foreach ($line in Get-Content -LiteralPath $envFile) {
    if ($line -eq '' -or $line.StartsWith('#')) { continue }
    $key, $value = $line -split '=', 2
    if ($null -eq $value) { throw "bad line in $envFile" }
    # Letters, digits and . _ / : @ + , - only: the values reach a YAML file.
    if ($value -notmatch '^[A-Za-z0-9._/:@+,-]*$') { throw "$key has characters that are not allowed" }
    if ($allowed -notcontains $key) { throw "unknown setting $key" }
    $s[$key] = $value
}
Remove-Item -LiteralPath $envFile -Force

if (-not $s['FORECREW_SERVER_URL']) { throw 'FORECREW_SERVER_URL is not set' }
$b, $w = $s['FORECREW_BOOTSTRAP_TOKEN'], $s['FORECREW_WORKER_TOKEN']
if ($b -and $w) { throw 'set a bootstrap token or a worker token, not both' }
if (-not ($b -or $w)) { throw 'no token was delivered' }

$labels = @()
if ($s['FORECREW_WORKER_LABELS']) { $labels += $s['FORECREW_WORKER_LABELS'] -split ',' | Where-Object { $_ } }
$labelsFile = Join-Path $root 'labels'
if (Test-Path $labelsFile) { $labels += Get-Content -LiteralPath $labelsFile | Where-Object { $_ -and -not $_.StartsWith('#') } }

$yaml = @("server_url: $($s['FORECREW_SERVER_URL'])")
if ($s['FORECREW_WORKER_NAME']) { $yaml += "name: $($s['FORECREW_WORKER_NAME'])" }
$maxJobs = if ($s['FORECREW_WORKER_MAX_JOBS']) { $s['FORECREW_WORKER_MAX_JOBS'] } else { '1' }
$yaml += "work_dir: '$workDir'", "max_jobs: $maxJobs"
if ($labels.Count -gt 0) { $yaml += 'labels:'; $yaml += $labels | ForEach-Object { "  - $_" } }
New-Item -ItemType Directory -Force -Path (Split-Path $config), $workDir | Out-Null
Set-Content -LiteralPath $config -Value $yaml -Encoding ascii

# Only the token reaches the worker's environment, not the settings.
if ($b) { $env:FORECREW_BOOTSTRAP_TOKEN = $b } else { $env:FORECREW_WORKER_TOKEN = $w }
$p = Start-Process -FilePath $worker -ArgumentList @('run', '--config', $config) -NoNewWindow -Wait -PassThru
Write-Output "forecrew-start: forecrew-worker ended with status $($p.ExitCode)"
Stop-Machine
exit $p.ExitCode
