# Powers a machine off at once: what a wall-time kill is. Found by its ID.
$vm = Get-VM -Id ([guid]$p.id) -ErrorAction SilentlyContinue
if ($vm -and $vm.State -ne 'Off') { Stop-VM -VM $vm -TurnOff -Force }
Write-Result @{ ok = $true }
