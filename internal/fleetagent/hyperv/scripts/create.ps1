# Makes a machine: a differencing disk of the base image, a small config disk
# that holds the bootstrap settings, a network device with its rules, and starts
# it. Whatever fails is removed again, so nothing is left behind.
Assert-Under $p.dir $p.vm_dir
$name = $p.name
$vm = $null # what this script made, and so the only machine it removes if it fails
$osDisk = Join-Path $p.dir 'os.vhdx'
$cfgDisk = Join-Path $p.dir 'config.vhdx'
$mount = Join-Path $p.dir 'mnt'
try {
    $base = Get-VHD -Path $p.base
    if ($base.Size -gt ([int64]$p.disk_gb * 1GB)) {
        throw ('the image''s disk is {0} GB, more than the shape''s {1} GB' -f [math]::Ceiling($base.Size / 1GB), $p.disk_gb)
    }
    # The base is shared by every machine and is never written.
    Set-ItemProperty -LiteralPath $p.base -Name IsReadOnly -Value $true
    New-Item -ItemType Directory -Force -Path $p.dir | Out-Null
    New-VHD -Path $osDisk -ParentPath $p.base -Differencing | Out-Null

    $vm = New-VM -Name $name -Path $p.dir -Generation $p.generation -MemoryStartupBytes ([int64]$p.memory_mb * 1MB) -VHDPath $osDisk -SwitchName $p.switch
    Set-VM -VM $vm -ProcessorCount $p.cpu -StaticMemory -CheckpointType Disabled -AutomaticStartAction Nothing -AutomaticStopAction TurnOff -Notes $p.notes
    Set-VMNetworkAdapter -VM $vm -MacAddressSpoofing Off -DhcpGuard On -RouterGuard On
    if ($p.vlan -gt 0) { Set-VMNetworkAdapterVlan -VM $vm -Access -VlanId $p.vlan }
    Set-MachineAcls $vm $p.rules

    # The config disk: its own small virtual disk, closed to everyone but the
    # machine, which wipes what it reads. It lives and goes with the machine.
    New-VHD -Path $cfgDisk -SizeBytes 32MB -Dynamic | Out-Null
    # Nothing is written to it before it is known that no other local user can read it.
    Assert-Closed $p.dir -AllowVirtualMachines
    Assert-Closed $cfgDisk -AllowVirtualMachines
    $disk = Mount-VHD -Path $cfgDisk -Passthru | Get-Disk
    try {
        Initialize-Disk -Number $disk.Number -PartitionStyle MBR
        $part = New-Partition -DiskNumber $disk.Number -UseMaximumSize
        Format-Volume -Partition $part -FileSystem NTFS -NewFileSystemLabel FCCONFIG -Confirm:$false | Out-Null
        New-Item -ItemType Directory -Force -Path $mount | Out-Null
        Add-PartitionAccessPath -DiskNumber $disk.Number -PartitionNumber $part.PartitionNumber -AccessPath $mount
        [IO.File]::WriteAllText((Join-Path $mount 'bootstrap.env'), $p.env, [Text.Encoding]::ASCII)
        Remove-PartitionAccessPath -DiskNumber $disk.Number -PartitionNumber $part.PartitionNumber -AccessPath $mount
    }
    finally {
        Dismount-VHD -Path $cfgDisk
        Remove-Item -LiteralPath $mount -Force -ErrorAction SilentlyContinue
    }
    Add-VMHardDiskDrive -VM $vm -ControllerType SCSI -Path $cfgDisk
    Start-VM -VM $vm
    Write-Result @{ id = $vm.Id.ToString() }
}
catch {
    $why = $_.Exception.Message
    if ($vm) {
        Stop-VM -VM $vm -TurnOff -Force -ErrorAction SilentlyContinue
        Remove-VM -VM $vm -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -LiteralPath $p.dir -Recurse -Force -ErrorAction SilentlyContinue
    throw $why
}
