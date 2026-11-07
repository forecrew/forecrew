<#
.SYNOPSIS
Installs, upgrades or removes forecrew-fleet-agent on Windows.

.DESCRIPTION
Run from an elevated PowerShell. The agent runs as the dedicated local user
forecrew-fleet-agent, a member of Hyper-V Administrators, started at boot by a
scheduled task that restarts it when it fails. It works out the kind of location
itself. Run the script again to upgrade the binary: the existing configuration
is kept. See docs/runbooks/add-provider.md.

Like forecrew-worker it is not a Windows service executable, so the script uses
a scheduled task, and "forecrew-fleet-agent doctor" runs as the administrator
running the script, not as the service user.

Credentials come from the environment, never from a parameter, which would show
in the process list and in the PowerShell history: FORECREW_JOIN_CODE from
"forecrew provider join", or FORECREW_PROVIDER_TOKEN from "forecrew provider
add", for scripts and air-gapped sites.

.PARAMETER Server
forecrewd's public URL. Required on the first run.

.PARAMETER Kind
hyperv, or another kind (default: detected).

.PARAMETER Version
Release to install, for example v0.2.0 (default: latest).

.PARAMETER BaseUrl
Release location (default https://github.com/forecrew/forecrew/releases).

.PARAMETER Reconfigure
Rewrite agent.yaml from these parameters; otherwise an existing one is kept.

.PARAMETER Uninstall
Stop and remove the task and the binary; keeps the configuration and the token,
and leaves machines running.

.PARAMETER Purge
With -Uninstall: also delete the configuration, the token and the service user.

.NOTES
For a private repository set the environment variable GITHUB_TOKEN (a token
that can read its releases): the release is then downloaded through the GitHub
API. It is never taken from a parameter and never written to disk, and it is
only sent to api.github.com.

.EXAMPLE
$env:FORECREW_JOIN_CODE = Read-Host 'Join code'
.\install.ps1 -Server https://forecrew.example.com
#>
[CmdletBinding()]
param(
    [string]$Server,
    [string]$Kind,
    [string]$Version = 'latest',
    [string]$BaseUrl = 'https://github.com/forecrew/forecrew/releases',
    [switch]$Reconfigure,
    [switch]$Uninstall,
    [switch]$Purge
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$taskName = 'forecrew-fleet-agent'
$userName = 'forecrew-fleet-agent'
$binDir = Join-Path $env:ProgramFiles 'Forecrew'
$bin = Join-Path $binDir 'forecrew-fleet-agent.exe'
$confDir = Join-Path $env:ProgramData 'forecrew-fleet-agent'
$conf = Join-Path $confDir 'agent.yaml'
$stateDir = Join-Path $confDir 'state'
$tokenFile = Join-Path $stateDir 'token'
$secretFile = Join-Path $confDir 'secret.txt'
$runScript = Join-Path $confDir 'run.ps1'
$token = $env:FORECREW_PROVIDER_TOKEN
$code = $env:FORECREW_JOIN_CODE

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

function Invoke-AgentShutdown {
    if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
    }
    Get-Process -Name 'forecrew-fleet-agent' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $bin } | Stop-Process -Force
}

if (-not (Test-Administrator)) { Exit-Install 'run this from an elevated PowerShell' }

# --- uninstall ---------------------------------------------------------------

if ($Uninstall) {
    Invoke-AgentShutdown
    if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) {
        Unregister-ScheduledTask -TaskName $taskName -Confirm:$false
    }
    Remove-Item -Path $bin -Force -ErrorAction SilentlyContinue
    Write-Info "removed the $taskName task and $bin"
    if ($Purge) {
        Remove-Item -Path $confDir -Recurse -Force -ErrorAction SilentlyContinue
        if (Get-LocalUser -Name $userName -ErrorAction SilentlyContinue) {
            Remove-LocalUser -Name $userName
        }
        Write-Info "deleted the configuration, the token and the user $userName"
    }
    else {
        Write-Info "kept $confDir; add -Purge to delete it"
    }
    return
}

# --- validate the arguments ----------------------------------------------------

if ($Purge) { Exit-Install '-Purge only goes with -Uninstall' }
Assert-SafeValue '-Server' $Server
Assert-SafeValue '-Kind' $Kind
Assert-SafeValue '-Version' $Version
Assert-SafeValue '-BaseUrl' $BaseUrl

$newConfig = $true
if ((Test-Path $conf) -and -not $Reconfigure) {
    $newConfig = $false
    if ($Server -or $Kind) { Write-Info "keeping the existing $conf; add -Reconfigure to rewrite it" }
}
if ($newConfig) {
    if (-not $Server) { Exit-Install '-Server is required' }
    if ($Server -notmatch '^https?://') { Exit-Install '-Server must be an http:// or https:// URL' }
}
if ($token -and $code) { Exit-Install 'set FORECREW_JOIN_CODE or FORECREW_PROVIDER_TOKEN, not both' }
if ($token) {
    if ($token -cnotmatch '^fcp_[A-Za-z0-9_-]+$') { Exit-Install 'the provider token must be fcp_ followed by letters, digits, - and _ (forecrew provider add prints it)' }
}
elseif ($code) {
    if ($code -notmatch '^[A-Za-z0-9-]+$') { Exit-Install 'a join code has letters, digits and - only (forecrew provider join prints it)' }
}
elseif (-not (Test-Path $secretFile) -and -not (Test-Path $tokenFile)) {
    Exit-Install 'set FORECREW_JOIN_CODE (forecrew provider join) or FORECREW_PROVIDER_TOKEN (forecrew provider add)'
}

# --- download --------------------------------------------------------------------

$arch = switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()) {
    'X64' { 'amd64' }
    'Arm64' { 'arm64' }
    default { Exit-Install 'unsupported architecture' }
}
$archive = "forecrew-fleet-agent_windows_$arch.zip"
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
    $newBin = Join-Path $tmp 'forecrew-fleet-agent.exe'

    # --- the kind of location ---------------------------------------------------------

    if ($newConfig) {
        if (-not $Kind) {
            $Kind = (& $newBin detect)
            if ($LASTEXITCODE -ne 0) { Exit-Install 'this machine fits no kind of location by itself; pass -Kind' }
            Write-Info "this machine looks like kind $Kind; pass -Kind to choose another"
        }
    }
    else {
        $Kind = Get-Content $conf | Where-Object { $_ -match '^kind:\s*(.+)$' } | ForEach-Object { $Matches[1] } | Select-Object -First 1
    }

    # --- user and files -------------------------------------------------------------

    if (-not (Get-LocalUser -Name $userName -ErrorAction SilentlyContinue)) {
        Write-Info "creating the user $userName"
        New-LocalUser -Name $userName -Password (Get-RandomPassword) -PasswordNeverExpires -UserMayNotChangePassword -Description 'Forecrew fleet agent' | Out-Null
    }
    if ($Kind -eq 'hyperv' -and (Get-LocalGroup -Name 'Hyper-V Administrators' -ErrorAction SilentlyContinue)) {
        Add-LocalGroupMember -Group 'Hyper-V Administrators' -Member $userName -ErrorAction SilentlyContinue
    }

    New-Item -ItemType Directory -Force -Path $confDir, $binDir, $stateDir | Out-Null
    & icacls.exe $stateDir /inheritance:r /grant:r "${userName}:(OI)(CI)F" 'SYSTEM:(OI)(CI)F' 'BUILTIN\Administrators:(OI)(CI)F' | Out-Null
    if ($LASTEXITCODE -ne 0) { Exit-Install "cannot restrict the permissions of $stateDir" }

    # Everyone may read the config and run.ps1; only the user, SYSTEM and
    # administrators read the secrets. New files replace the installed ones only
    # after "doctor" passes.
    if ($newConfig) {
        $lines = @("server_url: $Server", "kind: $Kind", "token_file: $($tokenFile -replace '\\', '/')")
        Set-Content -Path "$conf.tmp" -Value $lines -Encoding ascii
    }
    if ($token -or $code) {
        $secret = if ($token) { "FORECREW_PROVIDER_TOKEN=$token" } else { "FORECREW_JOIN_CODE=$code" }
        Set-Content -Path "$secretFile.tmp" -Value $secret -Encoding ascii -NoNewline
        & icacls.exe "$secretFile.tmp" /inheritance:r /grant:r "${userName}:R" 'SYSTEM:F' 'BUILTIN\Administrators:F' | Out-Null
        if ($LASTEXITCODE -ne 0) { Exit-Install "cannot restrict the permissions of $secretFile" }
    }

    # --- check, then replace the files and the binary ------------------------------------

    Write-Info 'running forecrew-fleet-agent doctor'
    & $newBin doctor --kind $Kind
    if ($LASTEXITCODE -ne 0) {
        Exit-Install 'forecrew-fleet-agent doctor found something missing; the installed service, configuration and token were not touched. Fix the problem above and run the script again'
    }

    Invoke-AgentShutdown
    Start-Sleep -Seconds 2
    if ($newConfig) {
        Write-Info "writing $conf"
        Move-Item -Path "$conf.tmp" -Destination $conf -Force
    }
    if ($token -or $code) {
        Write-Info "writing $secretFile"
        Move-Item -Path "$secretFile.tmp" -Destination $secretFile -Force
        # The credentials given now are the ones to use.
        Remove-Item -Path $tokenFile -Force -ErrorAction SilentlyContinue
    }
    Set-Content -Path $runScript -Encoding ascii -Value @(
        "if (Test-Path '$secretFile') {",
        "    `$name, `$value = (Get-Content -Raw -Path '$secretFile').Trim() -split '=', 2",
        '    [Environment]::SetEnvironmentVariable($name, $value)',
        '}',
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
    if ($code) { Write-Info 'now confirm it: forecrew provider joins, then forecrew provider confirm <id> --name <name> --facts <hash>' }
}
finally {
    Remove-Item -Path $tmp, "$conf.tmp", "$secretFile.tmp" -Recurse -Force -ErrorAction SilentlyContinue
}
