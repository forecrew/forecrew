# Provisions the Forecrew Windows image: Git and common toolchains from Chocolatey
# at pinned versions, the worker's start-up task, a clean network setup. The
# worker and forecrew-start.ps1 are already in C:\ProgramData\Forecrew.
$ErrorActionPreference = 'Stop'
$root = 'C:\ProgramData\Forecrew'

# Chocolatey's installer and the package versions are the ones you trust: pin
# them here. Versions below are what the image was last validated with.
Set-ExecutionPolicy Bypass -Scope Process -Force
[System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
if (-not (Get-Command choco -ErrorAction SilentlyContinue)) {
    Invoke-Expression ((New-Object System.Net.WebClient).DownloadString('https://community.chocolatey.org/install.ps1'))
}
$packages = [ordered]@{ git = '2.50.1'; golang = '1.24.5'; nodejs = '24.4.1'; python = '3.13.5'; jq = '1.7.1' }
foreach ($name in $packages.Keys) {
    choco install $name --version $packages[$name] -y --no-progress
    if ($LASTEXITCODE -ne 0) { throw "choco install $name failed" }
}
$env:Path = [System.Environment]::GetEnvironmentVariable('Path', 'Machine')

# Agent CLIs: exact versions from package.json, every dependency pinned by
# package-lock.json (uploaded by the template to $root\cli).
Push-Location (Join-Path $root 'cli')
npm ci --omit=dev --no-audit --no-fund
if ($LASTEXITCODE -ne 0) { throw 'npm ci failed' }
Pop-Location
$bin = Join-Path $root 'cli\node_modules\.bin'
[System.Environment]::SetEnvironmentVariable('Path', ([System.Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + $bin), 'Machine')
foreach ($cli in 'claude', 'codex', 'opencode') {
    & (Join-Path $bin "$cli.cmd") --version | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "$cli does not run" }
}

# Labels and contents for the manifest.
Set-Content -LiteralPath (Join-Path $root 'labels') -Value @('git', 'go', 'node', 'python', 'runtime:claude-code', 'runtime:codex', 'runtime:opencode') -Encoding ascii
$packages.GetEnumerator() | ForEach-Object { "$($_.Key)=$($_.Value)" } | Set-Content -LiteralPath (Join-Path $root 'packages') -Encoding ascii

# The worker starts at boot, reads what the provider delivered and ends the machine.
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -File $root\forecrew-start.ps1"
$trigger = New-ScheduledTaskTrigger -AtStartup
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -RunLevel Highest
Register-ScheduledTask -TaskName 'ForecrewWorker' -Action $action -Trigger $trigger -Principal $principal -Force | Out-Null
& "$root\forecrew-worker.exe" version
if ($LASTEXITCODE -ne 0) { throw 'forecrew-worker does not run' }

# The build's WinRM access does not travel with the image: Packer's shutdown
# command runs this last.
@'
winrm set winrm/config/service '@{AllowUnencrypted="false"}'
Disable-NetFirewallRule -DisplayName 'WinRM-HTTP' -ErrorAction SilentlyContinue
Remove-Item -LiteralPath $PSCommandPath -Force
shutdown /s /t 10 /f /d p:4:1 /c "Packer shutdown"
'@ | Set-Content -LiteralPath (Join-Path $root 'finish.ps1') -Encoding ascii
