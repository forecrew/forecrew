# Says what this host can give, and that what a provider needs is there.
if (-not (Get-Module -ListAvailable -Name Hyper-V)) { throw 'the Hyper-V PowerShell module is not installed' }
$vmms = Get-Service -Name vmms
if ($vmms.Status -ne 'Running') { throw 'the Hyper-V Virtual Machine Management service is not running' }
$vmhost = Get-VMHost
$os = Get-CimInstance Win32_OperatingSystem
New-Item -ItemType Directory -Force -Path $p.vm_dir | Out-Null
$drive = Get-PSDrive -Name $p.vm_dir.Substring(0, 1)
$switch = Get-VMSwitch -Name $p.switch -ErrorAction SilentlyContinue
Write-Result @{
    cpus = [int]$vmhost.LogicalProcessorCount
    memory_mb = [int]($os.TotalVisibleMemorySize / 1024)
    free_memory_mb = [int]($os.FreePhysicalMemory / 1024)
    free_disk_gb = [int]($drive.Free / 1GB)
    switch_found = [bool]$switch
    version = $os.Version
}
