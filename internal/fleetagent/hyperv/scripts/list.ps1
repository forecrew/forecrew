# Lists the machines whose name is Forecrew's, with their notes. The provider
# decides from the notes which really are.
$vms = @(Get-VM | Where-Object { $_.Name -like 'forecrew-*' } | ForEach-Object {
    @{ id = $_.Id.ToString(); name = $_.Name; state = $_.State.ToString(); notes = [string]$_.Notes }
})
Write-Result $vms
