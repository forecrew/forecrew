# Add and remove a location

A location (capacity provider) is a place machines come from. A fleet agent
runs there and only dials out to forecrewd. It needs:

- outbound HTTPS to the instance's `public_base_url`;
- what its kind needs, which `forecrew-fleet-agent doctor` lists.

| Kind | Runs on | Needs here |
|------|---------|------------|
| `microvm` | Linux with KVM | `/dev/kvm`, `firecracker` or `cloud-hypervisor` |
| `proxmox` | Linux next to Proxmox | an API token in a local file; the installer asks for it |
| `hyperv` | Windows | Hyper-V and `powershell.exe` |
| `tart` | Apple silicon | `tart` |
| `aws` | an EC2 instance in your account | its instance role |

## Add with a join code

Nobody sees a token this way, and the code alone gives access to nothing.

1. Draw a code. It works once, for 1 hour (`--cloud` gives it 7 days, when
   another team will run it):

   ```sh
   forecrew provider join
   ```

   On a server with several organizations `--org <slug>` is required, or
   `--org instance` for an instance-wide location, as an instance admin.

2. On the machine, run the install script as root. Keep the code off the command
   line, where every user of the machine could read it:

   ```sh
   curl -fsSLO https://raw.githubusercontent.com/forecrew/forecrew/main/deploy/fleet-agent/install.sh
   read -r FORECREW_JOIN_CODE && export FORECREW_JOIN_CODE   # paste the code, Enter
   sudo --preserve-env=FORECREW_JOIN_CODE,GITHUB_TOKEN sh install.sh --server https://forecrew.example.com
   ```

   The script works out the kind of location (`--kind` overrides it), asks here
   for what that kind needs locally, runs `doctor`, and starts the service. If
   `doctor` finds something missing, nothing installed was touched: fix it and
   run the script again. Where nobody can answer, set
   `FORECREW_PROXMOX_TOKEN` in the environment.

   On Windows, from an elevated PowerShell:

   ```powershell
   $env:FORECREW_JOIN_CODE = Read-Host 'Join code'
   .\install.ps1 -Server https://forecrew.example.com
   ```

3. Look at what it reported and confirm it. Check that the host, and for AWS the
   account, are the ones you expect: anyone holding the code could have reported
   this.

   ```sh
   forecrew provider joins
   forecrew provider confirm <id> --name home-server --facts <FACTS>
   ```

   `--facts` is the FACTS column of the report you read. If the agent sent
   another since, the confirmation is refused: read it again. The agent gets its
   token at its next call, within 5 seconds, and
   `forecrew providers` shows the location online.

If the agent restarts before you confirm, the code stays bound to the agent that
is gone: draw a new one and set it again with the install script.

## Add with a token

For scripts and air-gapped sites. The token is shown once:

```sh
forecrew provider add home-server --kind microvm
read -rs FORECREW_PROVIDER_TOKEN && export FORECREW_PROVIDER_TOKEN   # paste it, Enter
sudo --preserve-env=FORECREW_PROVIDER_TOKEN,GITHUB_TOKEN sh install.sh --server https://forecrew.example.com
```

## Check it

```sh
forecrew providers                        # online, status
forecrew-fleet-agent doctor               # on the machine
journalctl -u forecrew-fleet-agent -f     # Linux
tail -f /var/log/forecrew-fleet-agent.log # macOS
```

A location with no machine types offers nothing yet: add a shape for it.

## Upgrade

Run the script again; the configuration and token are kept. The restart leaves
machines running (`KillMode=process`), and the agent finds them again from its
provider.

## Remove

```sh
forecrew provider revoke home-server
sudo sh install.sh --uninstall            # --purge also deletes the configuration, token and user
```

Revoking stops the token at once and for good. forecrewd ends the rows of the
location's machines and requeues their jobs, but the agent stops without touching
them: destroy what is left at the place with its own tools. To let jobs finish
first, stop giving the location work and wait for `forecrew machines` to show
none there.

## When something is wrong

| You see | It means |
|---------|----------|
| `forecrewd rejected this agent` in the log, and the service stopped | The token is revoked or wrong, or the join code unknown, used or expired. Connect it again |
| `join code claimed by another agent` | Another agent used the code first, or this one restarted. Draw a new code. If you did not start that other agent, do not confirm its report |
| `too many join calls` (429) | Too many calls from one address, or too many unknown codes from anywhere. During a flood of wrong codes every join gets this, real codes too; connected locations are not touched. The agent backs off and tries again ([fleet.md](../fleet.md#limits-on-joining)) |
| `no contact with forecrewd` | Machines keep running and nothing new starts. It reconnects by itself |
| `this build of the fleet agent has no provider of kind ...` | The release does not serve that kind yet |
