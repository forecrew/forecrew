# Provisions the Forecrew Windows image: Git and common toolchains from
# Chocolatey at pinned versions, the agent CLIs, the worker's start-up task, and
# a finish script that turns the build's WinRM access off and runs sysprep
# /generalize. The worker, forecrew-start.ps1, package.json and sysprep.xml are
# already in C:\ProgramData\Forecrew. Needs CHOCOLATEY_INSTALLER_SHA256.
$ErrorActionPreference = 'Stop'
$root = 'C:\ProgramData\Forecrew'

# Only SYSTEM and administrators read the folder the provider delivers the
# bootstrap settings into (the file holds a one-time token).
icacls "$root\run" /inheritance:r /grant:r 'SYSTEM:(OI)(CI)F' 'Administrators:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'icacls failed' }

# Chocolatey: the version is pinned and the installer script must have the hash
# you reviewed; it is not run from the network as it comes.
$chocoVersion = '2.7.5'
$expected = $env:CHOCOLATEY_INSTALLER_SHA256
if (-not $expected) { throw 'CHOCOLATEY_INSTALLER_SHA256 is not set' }
[System.Net.ServicePointManager]::SecurityProtocol = [System.Net.ServicePointManager]::SecurityProtocol -bor 3072
$installer = Join-Path $env:TEMP 'choco-install.ps1'
Invoke-WebRequest -UseBasicParsing -Uri 'https://community.chocolatey.org/install.ps1' -OutFile $installer
if ((Get-FileHash -Algorithm SHA256 -LiteralPath $installer).Hash -ne $expected.ToUpper()) {
    throw 'the Chocolatey installer does not have the reviewed hash'
}
$env:chocolateyVersion = $chocoVersion
Set-ExecutionPolicy Bypass -Scope Process -Force
& $installer
if ($LASTEXITCODE -ne 0) { throw 'the Chocolatey install failed' }
Remove-Item -LiteralPath $installer -Force

# Package versions are pinned. They were the newest of each line when the
# template was written and are not exercised by Forecrew's CI, which has no
# Windows host: check them and bump them as you build.
$packages = [ordered]@{ git = '2.56.0.4'; golang = '1.27.3'; nodejs = '24.22.2'; python = '3.14.10'; jq = '1.8.2' }
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

# The QEMU guest agent and the virtio drivers, for an image a Proxmox location
# starts: the provider writes the bootstrap settings into the running machine
# through the agent. The installer is the one you supplied, and runs only if it
# has the hash you reviewed. Hyper-V needs none of this (GUEST_TOOLS is 0).
$tools = Join-Path $root 'guest-tools.exe'
if ($env:GUEST_TOOLS -eq '1') {
    if (-not $env:GUEST_TOOLS_SHA256) { throw 'GUEST_TOOLS_SHA256 is not set' }
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $tools).Hash -ne $env:GUEST_TOOLS_SHA256.ToUpper()) {
        throw 'the guest tools installer does not have the reviewed hash'
    }
    $install = Start-Process -FilePath $tools -ArgumentList '/install', '/quiet', '/norestart' -Wait -PassThru
    if ($install.ExitCode -ne 0 -and $install.ExitCode -ne 3010) { throw "the guest tools installer failed with $($install.ExitCode)" }
    if (-not (Get-Service -Name 'QEMU-GA' -ErrorAction SilentlyContinue)) { throw 'the QEMU guest agent service is not installed' }
    Set-Service -Name 'QEMU-GA' -StartupType Automatic
}
Remove-Item -LiteralPath $tools -Force

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

# Packer's shutdown command runs finish.ps1 last. It registers a one-shot
# scheduled task that runs as SYSTEM and starts it, then returns: a child of the
# WinRM shell would die with it when WinRM goes off, a scheduled task does not.
# The task removes the build's remote access, then runs sysprep: a new SID for
# every clone, and sysprep.xml gives the Administrator of each clone a random
# password and disables it, so no machine shares the build's password.
@'
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoProfile -ExecutionPolicy Bypass -File C:\ProgramData\Forecrew\sysprep-run.ps1'
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -RunLevel Highest
Register-ScheduledTask -TaskName 'ForecrewSysprep' -Action $action -Principal $principal -Force | Out-Null
Start-ScheduledTask -TaskName 'ForecrewSysprep'
'@ | Set-Content -LiteralPath (Join-Path $root 'finish.ps1') -Encoding ascii
@'
Start-Sleep -Seconds 10
winrm set winrm/config/service '@{AllowUnencrypted="false"}'
Disable-NetFirewallRule -DisplayName 'WinRM-HTTP' -ErrorAction SilentlyContinue
Stop-Service WinRM -Force
Set-Service WinRM -StartupType Disabled
Unregister-ScheduledTask -TaskName 'ForecrewSysprep' -Confirm:$false
& "$env:SystemRoot\System32\Sysprep\sysprep.exe" /generalize /oobe /shutdown /quiet /unattend:C:\ProgramData\Forecrew\sysprep.xml
'@ | Set-Content -LiteralPath (Join-Path $root 'sysprep-run.ps1') -Encoding ascii
