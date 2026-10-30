<#
.SYNOPSIS
Installs, upgrades or removes forecrew-worker on Windows.

.DESCRIPTION
Run from an elevated PowerShell. The worker runs as the dedicated local user
forecrew-worker, started at boot by a scheduled task that restarts it when it
fails. Run the script again to upgrade the binary: the existing configuration
is kept. See docs/runbooks/add-worker.md.

forecrew-worker is not a Windows service executable, so the script uses a
scheduled task instead of the service control manager. For the same reason
of practicality, "forecrew-worker check" runs as the administrator running the
script, not as the service user: it proves git, the runtime CLIs, the server and
the token, but not that the service user can reach them.

The token file is readable by the service user, and agents run as that user:
on Windows agents can read the worker token. (On Linux the token file is
root-only.)

On Windows the token file is readable by the service user, and agents run as
that user: agents can read the worker token. (On Linux the token file is
root-only.)

.PARAMETER Server
forecrewd's public URL. Required on the first run.

.PARAMETER Token
Worker token from "forecrew worker add". Defaults to the environment variable
FORECREW_WORKER_TOKEN, which is better than the parameter: a parameter shows in
the process list and in the PowerShell history. Required on the first run; a new
one replaces the old one.

.PARAMETER Name
Display name (default: the host name).

.PARAMETER Labels
Toolchains the machine offers, for example go,node,docker.

.PARAMETER WorkDir
Where job workspaces go (default C:\ProgramData\forecrew-worker\work).

.PARAMETER MaxJobs
Jobs at once (default 1; more only for repos you trust).

.PARAMETER Version
Release to install, for example v0.2.0 (default: latest).

.PARAMETER BaseUrl
Release location (default https://github.com/forecrew/forecrew/releases).

.PARAMETER Reconfigure
Rewrite worker.yaml from these parameters; otherwise an existing one is kept.

.PARAMETER Uninstall
Stop and remove the task and the binary; keeps the configuration, the token
and the work directory.

.PARAMETER Purge
With -Uninstall: also delete the configuration, the token, the work directory
and the service user.

.NOTES
For a private repository set the environment variable GITHUB_TOKEN (a token
that can read its releases): the release is then downloaded through the GitHub
API. It is never taken from a parameter and never written to disk, and it is
only sent to api.github.com.

.EXAMPLE
$secure = Read-Host -AsSecureString 'Worker token'
$env:FORECREW_WORKER_TOKEN = [System.Net.NetworkCredential]::new('', $secure).Password
.\install.ps1 -Server https://forecrew.example.com -Labels go,node
#>
[CmdletBinding()]
param(
    [string]$Server,
    [string]$Token = $env:FORECREW_WORKER_TOKEN,
    [string]$Name,
    [string[]]$Labels,
    [string]$WorkDir,
    [int]$MaxJobs,
    [string]$Version = 'latest',
    [string]$BaseUrl = 'https://github.com/forecrew/forecrew/releases',
    [switch]$Reconfigure,
    [switch]$Uninstall,
    [switch]$Purge
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$taskName = 'forecrew-worker'
$userName = 'forecrew-worker'
$binDir = Join-Path $env:ProgramFiles 'Forecrew'
$bin = Join-Path $binDir 'forecrew-worker.exe'
$confDir = Join-Path $env:ProgramData 'forecrew-worker'
$conf = Join-Path $confDir 'worker.yaml'
$tokenFile = Join-Path $confDir 'token.txt'
$runScript = Join-Path $confDir 'run.ps1'

function Write-Info([string]$Message) { Write-Output "install.ps1: $Message" }

function Exit-Install([string]$Message) { throw "install.ps1: $Message" }

# Values end up in YAML and in a command line.
function Assert-SafeValue([string]$What, [string]$Value) {
    if ($Value -and $Value -notmatch '^[A-Za-z0-9._/:@+\\-]+$') {
        Exit-Install "$What may contain letters, digits and . _ / : @ + \ - only"
    }
}

# A random password nobody needs to know: the scheduled task stores it.
function Get-RandomPassword {
    $alphabet = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!#%+-='
    $secure = New-Object System.Security.SecureString
    $random = New-Object byte[] 40
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($random)
    foreach ($b in $random) {
        $secure.AppendChar($alphabet[$b % $alphabet.Length])
    }
    $secure.AppendChar('a'); $secure.AppendChar('B'); $secure.AppendChar('7')
    return $secure
}

function Test-Administrator {
    $principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

if (-not (Test-Administrator)) { Exit-Install 'run this from an elevated PowerShell' }

# --- uninstall ---------------------------------------------------------------

if ($Uninstall) {
    if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    }
    Get-Process -Name 'forecrew-worker' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $bin } | Stop-Process -Force
    Remove-Item -Path $bin -Force -ErrorAction SilentlyContinue
    Write-Info "removed the $taskName task and $bin"
    if ($Purge) {
        Remove-Item -Path $confDir -Recurse -Force -ErrorAction SilentlyContinue
        if (Get-LocalUser -Name $userName -ErrorAction SilentlyContinue) {
            Remove-LocalUser -Name $userName
        }
        Write-Info "deleted the configuration, the token and the user $userName (a custom work directory stays)"
    }
    else {
        Write-Info "kept $confDir; add -Purge to delete it"
    }
    return
}

# --- validate the arguments ----------------------------------------------------

if ($Purge) { Exit-Install '-Purge only goes with -Uninstall' }
Assert-SafeValue '-Server' $Server
Assert-SafeValue '-Name' $Name
Assert-SafeValue '-WorkDir' $WorkDir
Assert-SafeValue '-Version' $Version
Assert-SafeValue '-BaseUrl' $BaseUrl
foreach ($label in $Labels) { Assert-SafeValue '-Labels' $label }

$newConfig = $true
if ((Test-Path $conf) -and -not $Reconfigure) {
    $newConfig = $false
    if ($Server -or $Name -or $Labels -or $WorkDir -or $PSBoundParameters.ContainsKey('MaxJobs')) {
        Write-Info "keeping the existing $conf; add -Reconfigure to rewrite it"
    }
}
if ($newConfig) {
    if (-not $Server) { Exit-Install '-Server is required' }
    if ($Server -notmatch '^https?://') { Exit-Install '-Server must be an http:// or https:// URL' }
    if (-not $WorkDir) { $WorkDir = Join-Path $confDir 'work' }
    if ($PSBoundParameters.ContainsKey('MaxJobs')) {
        if ($MaxJobs -lt 1) { Exit-Install '-MaxJobs must be a positive number' }
        if ($MaxJobs -gt 1) {
            Write-Info "warning: with -MaxJobs above 1 the agents of concurrent jobs can read each other's repo tokens; use it only for repos you trust equally"
        }
    }
}
if ($Token) {
    if ($Token -cnotmatch '^fcw_[A-Za-z0-9_-]+$') { Exit-Install 'the worker token must be fcw_ followed by letters, digits, - and _ (forecrew worker add prints it)' }
}
elseif (-not (Test-Path $tokenFile)) {
    Exit-Install 'a worker token is required: pass -Token or set FORECREW_WORKER_TOKEN'
}

# --- download --------------------------------------------------------------------

$arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    'X64' { 'amd64' }
    'Arm64' { 'arm64' }
    default { Exit-Install 'unsupported architecture' }
}
$archive = "forecrew-worker_windows_$arch.zip"
$urlDir = if ($Version -eq 'latest') { "$BaseUrl/latest/download" } else { "$BaseUrl/download/$Version" }

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ([System.IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Info "downloading $archive ($Version)"
    $ProgressPreference = 'SilentlyContinue'
    $githubRepo = $null
    if ($BaseUrl -match '^https://github\.com/([^/]+/[^/]+)/releases$') { $githubRepo = $Matches[1] }
    $useApi = $false
    if ($env:GITHUB_TOKEN) {
        if ($env:GITHUB_TOKEN -notmatch '^[A-Za-z0-9_-]+$') { Exit-Install 'GITHUB_TOKEN has unexpected characters' }
        if ($githubRepo) { $useApi = $true }
        else { Write-Info 'ignoring GITHUB_TOKEN: -BaseUrl is not a github.com releases URL' }
    }
    if ($useApi) {
        $apiHeaders = @{ Authorization = "Bearer $env:GITHUB_TOKEN"; Accept = 'application/vnd.github+json' }
        $assetHeaders = @{ Authorization = "Bearer $env:GITHUB_TOKEN"; Accept = 'application/octet-stream' }
        $release = if ($Version -eq 'latest') { 'latest' } else { "tags/$Version" }
        $releaseInfo = Invoke-RestMethod -UseBasicParsing -Headers $apiHeaders -Uri "https://api.github.com/repos/$githubRepo/releases/$release"
    }
    foreach ($file in @($archive, 'checksums.txt')) {
        $target = Join-Path $tmp $file
        if (-not $useApi) {
            Invoke-WebRequest -UseBasicParsing -Uri "$urlDir/$file" -OutFile $target
            continue
        }
        $asset = $releaseInfo.assets | Where-Object { $_.name -eq $file } | Select-Object -First 1
        if (-not $asset) { Exit-Install "the release has no asset $file" }
        # The API answers with a redirect to a signed URL on another host. Follow
        # it by hand so the token is only ever sent to api.github.com.
        $location = $null
        try {
            Invoke-WebRequest -UseBasicParsing -Headers $assetHeaders -Uri $asset.url -MaximumRedirection 0 -ErrorAction Stop | Out-Null
        }
        catch {
            $response = $_.Exception.Response
            if (-not $response) { throw }
            $location = if ($response.Headers -is [System.Net.WebHeaderCollection]) { $response.Headers['Location'] } else { $response.Headers.Location.AbsoluteUri }
        }
        if (-not $location) { Exit-Install "the GitHub API did not redirect the download of $file" }
        Invoke-WebRequest -UseBasicParsing -Uri $location -OutFile $target
    }
    $line = Get-Content (Join-Path $tmp 'checksums.txt') | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" } | Select-Object -First 1
    if (-not $line) { Exit-Install "checksums.txt has no entry for $archive" }
    $want = ($line -split '\s+')[0]
    $have = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmp $archive)).Hash
    if ($want -ne $have) { Exit-Install "checksum mismatch for $archive" }
    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force

    # --- user and files -------------------------------------------------------------

    if (-not (Get-LocalUser -Name $userName -ErrorAction SilentlyContinue)) {
        Write-Info "creating the user $userName"
        New-LocalUser -Name $userName -Password (Get-RandomPassword) -PasswordNeverExpires -UserMayNotChangePassword -Description 'Forecrew worker' | Out-Null
    }

    New-Item -ItemType Directory -Force -Path $confDir, $binDir | Out-Null
    # Everyone may read the config and run.ps1; only the user, SYSTEM and
    # administrators read the token. The new config and token go to temporary
    # files and replace the installed ones only after "check" passes.
    $confCheck = $conf
    if ($newConfig) {
        $lines = @("server_url: $Server")
        if ($Name) { $lines += "name: $Name" }
        if ($Labels) { $lines += 'labels: [' + (($Labels | ForEach-Object { $_ -split ',' } | Where-Object { $_ }) -join ', ') + ']' }
        $lines += "work_dir: $($WorkDir -replace '\\', '/')"
        if ($PSBoundParameters.ContainsKey('MaxJobs')) { $lines += "max_jobs: $MaxJobs" }
        Set-Content -Path "$conf.tmp" -Value $lines -Encoding ascii
        $confCheck = "$conf.tmp"
    }
    $workDirPath = (Get-Content $confCheck | Where-Object { $_ -match '^work_dir:\s*(.+)$' } | ForEach-Object { $Matches[1] } | Select-Object -First 1) -replace '/', '\'
    New-Item -ItemType Directory -Force -Path $workDirPath | Out-Null
    & icacls.exe $workDirPath /grant "${userName}:(OI)(CI)F" | Out-Null

    if ($Token) {
        Set-Content -Path "$tokenFile.tmp" -Value $Token -Encoding ascii -NoNewline
        & icacls.exe "$tokenFile.tmp" /inheritance:r /grant:r "${userName}:R" 'SYSTEM:F' 'BUILTIN\Administrators:F' | Out-Null
        if ($LASTEXITCODE -ne 0) { Exit-Install "cannot restrict the permissions of $tokenFile" }
        $checkToken = $Token
    }
    else {
        $checkToken = (Get-Content -Raw -Path $tokenFile).Trim()
        if ($checkToken -cnotmatch '^fcw_[A-Za-z0-9_-]+$') { Exit-Install "the token in $tokenFile is not a worker token; pass a new one" }
    }

    # --- check, then replace the files and the binary ------------------------------------

    $newBin = Join-Path $tmp 'forecrew-worker.exe'
    Write-Info 'running forecrew-worker check'
    $env:FORECREW_WORKER_TOKEN = $checkToken
    try {
        & $newBin check --config $confCheck
        $checkExit = $LASTEXITCODE
    }
    finally {
        Remove-Item Env:\FORECREW_WORKER_TOKEN -ErrorAction SilentlyContinue
    }
    if ($checkExit -ne 0) {
        Exit-Install 'forecrew-worker check failed; the installed service, configuration and token were not touched. Fix the problem above and run the script again'
    }

    # Only now stop the worker: the task, then the process itself, which the task
    # may leave behind.
    if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    }
    Get-Process -Name 'forecrew-worker' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $bin } | Stop-Process -Force
    Start-Sleep -Seconds 2
    if ($newConfig) {
        Write-Info "writing $conf"
        Move-Item -Path "$conf.tmp" -Destination $conf -Force
    }
    if ($Token) {
        Write-Info "writing $tokenFile"
        Move-Item -Path "$tokenFile.tmp" -Destination $tokenFile -Force
    }
    Set-Content -Path $runScript -Encoding ascii -Value @(
        "`$env:FORECREW_WORKER_TOKEN = (Get-Content -Raw -Path '$tokenFile').Trim()",
        "& '$bin' run --config '$conf'",
        'exit $LASTEXITCODE'
    )
    Copy-Item -Path $newBin -Destination $bin -Force

    # --- scheduled task -------------------------------------------------------------------

    # A fresh password on every run; the task stores it.
    $securePassword = Get-RandomPassword
    Set-LocalUser -Name $userName -Password $securePassword
    $password = (New-Object System.Management.Automation.PSCredential($userName, $securePassword)).GetNetworkCredential().Password
    $action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$runScript`""
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
    Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Settings $settings -User $userName -Password $password -RunLevel Limited -Force | Out-Null
    Start-ScheduledTask -TaskName $taskName
    Write-Info "the $taskName task is running: Get-ScheduledTask -TaskName $taskName"
}
finally {
    Remove-Item -Path $tmp, "$conf.tmp", "$tokenFile.tmp" -Recurse -Force -ErrorAction SilentlyContinue
}
