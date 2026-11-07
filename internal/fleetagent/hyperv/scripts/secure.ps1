# Closes the directories the provider keeps its scripts, images and machines in to
# SYSTEM, Administrators and the agent's own account, and fails if they are open
# to anyone else. It runs from the agent's memory, not from a file, so that it
# cannot be replaced in a directory that was open. A machine's config disk, which
# holds a token, is under them.
$system = New-Object Security.Principal.SecurityIdentifier 'S-1-5-18'
$admins = New-Object Security.Principal.SecurityIdentifier 'S-1-5-32-544'
$me = [Security.Principal.WindowsIdentity]::GetCurrent().User
foreach ($dir in @($p.dirs)) {
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $acl = New-Object Security.AccessControl.DirectorySecurity
    $acl.SetAccessRuleProtection($true, $false)
    foreach ($sid in @($system, $admins, $me)) {
        $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($sid, 'FullControl', 'ContainerInherit,ObjectInherit', 'None', 'Allow')))
    }
    try { Set-Acl -LiteralPath $dir -AclObject $acl } catch { }
    Assert-Closed $dir
}
Write-Result @{ ok = $true }
