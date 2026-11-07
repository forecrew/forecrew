# Starts forecrew-worker on a Windows Forecrew machine from the settings its
# provider delivered, as forecrew-start does on Linux. The settings are KEY=value
# lines in C:\ProgramData\Forecrew\run\bootstrap.env (see forecrew-start for the
# keys). The file is read as data, deleted once read, and the machine powers off
# when the worker ends: it is thrown away after its jobs. On Windows the folder
# is on the machine's own disk, not in RAM: only SYSTEM and administrators may
# read it (the ACL set here and at image build). The provider delivers the
# settings into the machine, never in the image or on a host share: Hyper-V on
# a config disk of the machine's own, Proxmox through the QEMU guest agent (the
# image turns WinRM off). The disk is destroyed with the machine.
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

# The settings reach the machine in one of two ways, both before this script
# reads them: Hyper-V gives the machine a small disk labelled FCCONFIG with
# bootstrap.env on it (the disk is the machine's own, made for it and removed
# with it), and a Proxmox location writes the file through the QEMU guest agent.
# Neither is in the image.

# Moves bootstrap.env from the config disk, when there is one, to $envFile, and
# wipes it from the disk. Disks of the machine that are offline are brought
# online first: a new disk may be offline until someone says otherwise.
function Import-ConfigDisk {
    if (-not (Get-Command Get-Volume -ErrorAction SilentlyContinue)) { return }
    Get-Disk -ErrorAction SilentlyContinue | Where-Object { $_.IsOffline } | ForEach-Object { Set-Disk -Number $_.Number -IsOffline $false -ErrorAction SilentlyContinue }
    $volume = Get-Volume -FileSystemLabel 'FCCONFIG' -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $volume) { return }
    if (-not $volume.DriveLetter) {
        $partition = Get-Partition -ErrorAction SilentlyContinue | Where-Object { (Get-Volume -Partition $_ -ErrorAction SilentlyContinue).FileSystemLabel -eq 'FCCONFIG' } | Select-Object -First 1
        if (-not $partition) { return }
        Add-PartitionAccessPath -DiskNumber $partition.DiskNumber -PartitionNumber $partition.PartitionNumber -AssignDriveLetter -ErrorAction SilentlyContinue
        $volume = Get-Volume -FileSystemLabel 'FCCONFIG' -ErrorAction SilentlyContinue | Select-Object -First 1
        if (-not $volume.DriveLetter) { return }
    }
    $source = "$($volume.DriveLetter):\bootstrap.env"
    if (-not (Test-Path -LiteralPath $source)) { return }
    New-Item -ItemType Directory -Force -Path (Split-Path $envFile) | Out-Null
    Copy-Item -LiteralPath $source -Destination $envFile -Force
    # Overwrite, then delete: the token is not left on the disk.
    $length = (Get-Item -LiteralPath $source).Length
    [IO.File]::WriteAllBytes($source, (New-Object byte[] $length))
    Remove-Item -LiteralPath $source -Force
}

# A file the guest agent is still writing is open for writing: it can be read
# only when nobody else holds it.
function Test-Complete([string]$path) {
    if (-not (Test-Path -LiteralPath $path)) { return $false }
    try {
        $stream = [IO.File]::Open($path, 'Open', 'Read', 'None')
        $complete = $stream.Length -gt 0
        $stream.Dispose()
        return $complete
    }
    catch { return $false }
}

# The provider delivers the settings around boot: wait for them for a while.
$deadline = (Get-Date).AddMinutes(10)
while (-not (Test-Complete $envFile) -and (Get-Date) -lt $deadline) {
    Import-ConfigDisk
    if (-not (Test-Complete $envFile)) { Start-Sleep -Seconds 2 }
}
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
