# Removes a machine, its disks and its directory. The machine is found by its ID,
# never by its name, which an operator's own VM may share; with no ID only the
# directory goes. Each step is safe to repeat, and a machine that is gone is no
# error.
Assert-Under $p.dir $p.vm_dir
if ($p.id) {
    $vm = Get-VM -Id ([guid]$p.id) -ErrorAction SilentlyContinue
    if ($vm) {
        if ($vm.State -ne 'Off') { Stop-VM -VM $vm -TurnOff -Force }
        Remove-VM -VM $vm -Force
    }
}
if (Test-Path -LiteralPath $p.dir) { Remove-Item -LiteralPath $p.dir -Recurse -Force }
Write-Result @{ ok = $true }
