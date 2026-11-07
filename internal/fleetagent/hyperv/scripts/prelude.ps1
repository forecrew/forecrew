# Start of every script the Hyper-V provider runs. The provider sends the
# parameters as one line of JSON on standard input: nothing it is given is put
# into a command line or into the text of a script, so no value can be run as
# code. A script writes one JSON value on its last line of standard output, or
# fails with a message on standard error and exit code 1.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
trap { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }
$p = [Console]::In.ReadToEnd() | ConvertFrom-Json

function Write-Result($value) {
    [Console]::Out.WriteLine((ConvertTo-Json -InputObject $value -Depth 6 -Compress))
}

# A path the provider names must be inside the directory it was given for
# virtual machines: a script never removes anything elsewhere.
function Assert-Under([string]$path, [string]$root) {
    $full = [IO.Path]::GetFullPath($path)
    $base = [IO.Path]::GetFullPath($root).TrimEnd('\') + '\'
    if (-not $full.StartsWith($base, [StringComparison]::OrdinalIgnoreCase)) {
        throw "$path is not inside $root"
    }
}

# Fails if anything but SYSTEM, Administrators and the agent's own account (and,
# where asked, the virtual accounts Hyper-V gives a machine on its own files) can
# reach a path. Nothing that holds a token may be open to another local user.
function Assert-Closed([string]$path, [switch]$AllowVirtualMachines) {
    $allowed = @('S-1-5-18', 'S-1-5-32-544', [Security.Principal.WindowsIdentity]::GetCurrent().User.Value)
    foreach ($rule in (Get-Acl -LiteralPath $path).Access) {
        if ($rule.AccessControlType -ne 'Allow') { continue }
        try { $sid = $rule.IdentityReference.Translate([Security.Principal.SecurityIdentifier]).Value }
        catch { $sid = [string]$rule.IdentityReference }
        if ($allowed -contains $sid) { continue }
        if ($AllowVirtualMachines -and $sid.StartsWith('S-1-5-83-')) { continue }
        throw "$path is open to $($rule.IdentityReference): only SYSTEM, Administrators and the agent's account may have access"
    }
}

# Network rules of a machine (a VM object, so that another VM of the same name
# is never touched), from the provider's list of outbound rules in the order they
# apply. Hyper-V evaluates higher weights first, so the first rule gets the
# highest. Rules for a protocol and port (forecrewd, DNS) are stateful, which is
# what lets their answers in. They are not mirrored inbound: there -RemotePort is
# the sender's own port, and a mirror would let anything that sends from port 53
# or 443 reach any port of the machine. The same goes for any inbound Allow: it
# must name the machine's own port (-LocalPort), or not exist. So an allowlist
# entry is allowed outbound only, as stateful TCP and UDP rules, which let its
# answers in; only the denials are mirrored inbound, so that no other private
# address may start a conversation with the machine. IPv6 is closed both ways at
# the lowest weight: machines have IPv4 only.
function Set-MachineAcls($vm, $rules) {
    Get-VMNetworkAdapterExtendedAcl -VM $vm | Remove-VMNetworkAdapterExtendedAcl
    $add = {
        param($direction, $rule, $weight)
        $a = @{
            VM = $vm; Direction = $direction; Weight = $weight
            Action = $(if ($rule.allow) { 'Allow' } else { 'Deny' })
            RemoteIPAddress = $rule.dest
        }
        if ($rule.port -gt 0) { $a.RemotePort = [string]$rule.port }
        if ($rule.proto) { $a.Protocol = $rule.proto.ToUpper(); $a.Stateful = $true }
        elseif ($direction -eq 'Outbound' -and $rule.allow) {
            # Allowed to anywhere on a network, in both protocols, so that the answers come back.
            foreach ($protocol in 'TCP', 'UDP') { $a.Protocol = $protocol; $a.Stateful = $true; Add-VMNetworkAdapterExtendedAcl @a | Out-Null }
            return
        }
        Add-VMNetworkAdapterExtendedAcl @a | Out-Null
    }
    # Asking for an address by DHCP must work whatever else is closed. The reply is
    # a datagram from the server's port 67 to the client's port 68, and only to 68:
    # a broadcast request has no tracked answer, so it is allowed by its ports.
    Add-VMNetworkAdapterExtendedAcl -VM $vm -Direction Outbound -Action Allow -Protocol UDP -RemoteIPAddress 255.255.255.255 -RemotePort 67 -Weight 30000 | Out-Null
    Add-VMNetworkAdapterExtendedAcl -VM $vm -Direction Inbound -Action Allow -Protocol UDP -LocalPort 68 -RemotePort 67 -Weight 30000 | Out-Null
    $weight = 20000
    foreach ($rule in @($rules)) {
        & $add 'Outbound' $rule $weight
        if (-not $rule.proto -and -not $rule.allow) { & $add 'Inbound' $rule $weight }
        $weight--
    }
    foreach ($direction in 'Outbound', 'Inbound') {
        Add-VMNetworkAdapterExtendedAcl -VM $vm -Direction $direction -Action Deny -RemoteIPAddress '::/0' -Weight 1 | Out-Null
    }
}
