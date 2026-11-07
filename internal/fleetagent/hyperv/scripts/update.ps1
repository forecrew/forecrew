# Applies new notes and, when given, new network rules to a running machine, found
# by its ID: names are not unique in Hyper-V.
$vm = Get-VM -Id ([guid]$p.id)
Set-VM -VM $vm -Notes $p.notes
if ($p.apply_rules) { Set-MachineAcls $vm $p.rules }
Write-Result @{ ok = $true }
