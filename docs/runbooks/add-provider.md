# Add and remove a location

A location (capacity provider) is a place machines come from. A fleet agent
runs there and only dials out to forecrewd. It needs:

- outbound HTTPS to the instance's `public_base_url`;
- what its kind needs, which `forecrew-fleet-agent doctor` lists.

| Kind | Runs on | Needs here |
|------|---------|------------|
| `microvm` | Linux with KVM | root, `/dev/kvm`, `firecracker` and `jailer` (or `cloud-hypervisor`), `ip`, `nft`, IP forwarding, trusted image keys ([fleet.md](../fleet.md#home-server-setup)) |
| `proxmox` | Linux next to Proxmox | an API token in a local file (the installer asks for it), the datacenter firewall on, a template per image ([fleet.md](../fleet.md#proxmox-locations)) |
| `hyperv` | Windows | Hyper-V, `powershell.exe`, a virtual switch, trusted image keys ([fleet.md](../fleet.md#hyper-v-locations)) |
| `tart` | Apple silicon | `tart` and `softnet` on PATH, a Mac that never sleeps, trusted image keys ([fleet.md](../fleet.md#macos-locations-tart)) |
| `aws` | an EC2 instance in your account | its instance role; the kit in [byoc-aws.md](byoc-aws.md) makes both |

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

## Add a Mac (Tart)

For a Mac mini at home or a rented Mac. Read the limits first
([fleet.md](../fleet.md#macos-locations-tart)): **two VMs per Mac, at most**, so a
Mac offers two machines at once; a **rented Mac is paid for 24 hours at least**
(AWS requires 24 hours before a Mac Dedicated Host is released; Apple's licence
requires 24 consecutive hours for any lease of a Mac); and Forecrew runs your
organization's own jobs on it, never anyone else's by the minute.

1. On the Mac, as an administrator: no sleep (`sudo pmset -a sleep 0`), enough
   disk (the base image is tens of GB, and each job's clone grows from it; the
   default floor is 100 GB free), and Tart and Softnet installed from sources you
   trust, with Softnet allowed to run (setuid or passwordless sudo).
2. Build the image on a Mac ([images.md](../images.md#windows-and-macos-builds):
   `deploy/images/macos`, a base VM you pinned by digest), push it to a registry you
   control, write its manifest with `--format tart`, sign it with your key off the
   Mac and register it (`forecrew image add`). Put your public key in
   `/etc/forecrew-fleet-agent/image-keys.pub` on the Mac.
3. List the registry your images come from in `/etc/forecrew-fleet-agent/tart.yaml`
   (`registry_hosts: [ghcr.io]`): an image from another host is refused. A
   private registry: also add `registry_username: env:NAME` and
   `registry_password: env:NAME` (credentials need `registry_hosts`) and put
   `NAME=value` lines in the agent's `secrets.env` (`/etc/forecrew-fleet-agent`, mode
   0600, which the launchd job reads). Credentials stay on the Mac.
4. Join as above (`forecrew provider join`, then the install script on the Mac;
   it detects `tart`). On a rented Mac the same steps run on the rented host:
   connect to it, install, and release the host only after `forecrew machines`
   shows none there and the 24 hours have passed.
5. `forecrew-fleet-agent doctor --kind tart` must show no `fail`. Confirm the
   report as above, then add a shape:

   ```yaml
   kind: shape
   name: mac-xcode16
   provider: home-mac
   os: macos
   arch: arm64
   cpu: 4
   memory_mb: 8192
   disk_gb: 80
   image: macos
   labels: [xcode:16]       # what the image's contents.labels list
   max: 2                   # never more than the Mac's two
   max_jobs: 1
   note: "Mac mini, two at once, boots in about a minute"
   ```

   A job that needs it says `needs: [xcode:16]`.

### Live test of a Mac

Not run yet: the provider was written and tested on Linux against a fake `tart`
(`internal/fleetagent/tart/testdata/tart`) that follows Tart's documented commands.
On a real Mac, in this order, and stop at the first failure:

1. `forecrew-fleet-agent doctor --kind tart`: all `ok`.
2. By hand, as the agent's user (`sudo -u _forecrew-fleet-agent env HOME=/var/lib/forecrew-fleet-agent TART_HOME=/var/lib/forecrew-fleet-agent/tart/tart-home ...`):
   `tart pull <registry>/<name>@sha256:<digest>`; `tart clone` of it to `probe`;
   `tart run --no-graphics --net-softnet probe` in another shell; `tart exec probe /usr/bin/true`;
   `printf 'x=1\n' | tart exec -i probe sudo -n /bin/sh -c 'cat >/tmp/t' sh`; `tart stop probe`; `tart delete probe`.
3. From forecrewd: add the shape, queue a job that needs it, and watch `forecrew machines`: one machine
   reaches `ready`, runs the job, and the VM is gone (`sudo -u ... tart list`).
4. Queue three jobs at once: only two machines run; the third waits.
5. From inside a machine (`tart exec` as the agent's user into a running one), try to reach the Mac:
   `curl` and `ssh` to its LAN address, to the **gateway of the machine's network** (`netstat -rn` in the guest) and to
   the Mac's **public address**, the one your router forwards (`curl ifconfig.me` on the Mac, then that address from
   the guest); also any address in `100.64.0.0/10` the Mac has (Tailscale), on every port the Mac serves. Every one
   must fail. A public address of another host works, and so does a name (DNS goes to `dns`, not to the Mac).
6. Kill the agent mid-job (`launchctl kickstart -k system/com.forecrew.fleet-agent`): the machine keeps
   running, and the restarted agent lists it.
7. Revoke the location while a machine runs, destroy the leftover by hand
   (`tart delete`), and check no `forecrew-*` VM remains.

What none of this has proven yet, and the first run on a Mac must settle:
`tart pull` and `tart clone` accept `registry/name@sha256:...`; Tart caches a pulled
image at `TART_HOME/cache/OCIs/<registry>/<repository>/<digest>/` with `manifest.json`
holding the registry's bytes (so it hashes to the digest), `config.json`, `nvram.bin` and
`disk.img`, and the manifest's disk layers carry the annotations
`org.cirruslabs.tart.uncompressed-size` and `...uncompressed-content-digest`, in disk
order, covering `disk.img` exactly (else every pull is refused: look at the error, it says
which check); the process name of a running VM in `ps -axo comm=` is
`com.apple.Virtualization.VirtualMachine`, one per VM (the count of the Mac's VMs, and so
Apple's limit, depends on it); `tart ip --resolver agent` answers the machine's address, and the Mac has an address on that
network (the check refuses every machine otherwise); `--net-softnet-block=@host,<address>/32,...`
is accepted by the Softnet release you run, `@host` meaning the Mac's address on the machine's
own network, and blocks the Mac although Softnet allows its gateway and its public addresses;
`softnet --version` prints a version number, and 0.25.0 is new enough (the minimum is a
choice); Tart's own message when the limit of VMs is reached contains "exceeds the system limit";
a guest whose DNS is set with
`networksetup -setdnsservers` resolves through it, and the base VM's network services
are the ones `networksetup -listallnetworkservices` lists; `tart list --format json`
has the fields the provider reads (`Name`, `Running`, `Disk`); `tart exec` passes
flags such as `-n` and `-c` to the guest command without a `--`; the admin user of the
guest has passwordless `sudo` and the guest agent runs in the image; `tart stop` on a
VM and `tart delete` afterwards behave as the fake's; Softnet blocks the LAN and the host
and `--net-softnet-allow` takes a comma-separated list; `tart set --disk-size` grows
the guest's volume (or the image does it at boot); the macOS 26 requirement of
Softnet; the boot time to the guest agent; the 2.27.3 minimum version; and that
`pmset -g` prints `sleep 0` as the check expects.

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

## Proxmox and Hyper-V: first machine, and what was tested

Do this once per host, before relying on it. Everything here needs a real
Proxmox VE or Hyper-V host; **none of it was run on one when the providers were
written**: they are tested against a fake Proxmox API and a fake PowerShell host
(`internal/testkit`) built from the documented interfaces.

### Proxmox VE

1. On the node, create a role and an API token for the agent (privileges in
   [fleet.md](../fleet.md#proxmox-locations)), and run the installer
   (`FORECREW_PROXMOX_TOKEN` or its prompt). Turn on the datacenter firewall.
2. Make a Linux template: build the qcow2 image (`deploy/images/build.sh qcow2`),
   sign and register it, import the file (`qm importdisk <vmid> image.qcow2
   <storage>`, attach it as `scsi0`, boot order, `agent: 1`), convert it with
   `qm template <vmid>`, and tag it `forecrew-image-<first 32 hex digits of the
   digest>` (`qm set <vmid> --tags forecrew-image-...`). A Windows template is the
   same with the image from `forecrew-fleet-agent build-image --guest-tools ...`,
   attached as `sata0` with an `e1000` device.
3. Put the public key you signed with in `/etc/forecrew-fleet-agent/image-keys.pub`
   and run `forecrew-fleet-agent doctor --kind proxmox`.
4. Add a shape on the location whose `provider_config` is
   `{"node": "<node>", "template": <vmid>}` and run a job on it. Then check, in
   the Proxmox web UI: the VM is named `forecrew-...`, its network device is on the
   bridge you set with the firewall on, its rules start with forecrewd's address
   and end with the private ranges, its notes hold the machine's tags and no
   token, and it is gone, disks included, when the job ends. From inside the VM
   try the LAN and a metadata address: both must fail.

### Hyper-V

1. On the Windows host run `install.ps1` from an elevated PowerShell, create
   `C:\ProgramData\forecrew-fleet-agent\hyperv.yaml` with at least `switch`, and
   put the public key you sign images with in `image-keys.pub` there.
2. Build the image on the host: `forecrew-fleet-agent build-image ... --cache-dir
   <image_dir>` (about an hour; follow `progress.json`), check and sign the
   manifest on another computer, and register it with `forecrew image add`.
3. `forecrew-fleet-agent doctor --kind hyperv`, add a Windows shape, run a job.
   Check in Hyper-V Manager and PowerShell: the VM `forecrew-...` has a
   differencing disk of the base `vhdx`, no checkpoint, a config disk that is empty
   of `bootstrap.env` once the worker is up, `Get-VMNetworkAdapterExtendedAcl`
   shows the rules, and VM, disks and directory are gone when the job ends.

### What was not run on a real host

| Not verified live | Why it matters |
|-------------------|----------------|
| Every call of the Proxmox API against Proxmox VE | The fake follows the API viewer of Proxmox VE 9 (parameters, privileges, answers); real task logs, error texts and timing may differ |
| That a new firewall rule goes first, and the rules' effect | The provider reads the order back and corrects it, and fails if it cannot; whether the rules block the LAN is for you to try |
| The guest agent's `file-write` into `/run/forecrew` and `C:\ProgramData\Forecrew\run`, and the worker starting from `forecrew-worker.path` | Needs a real guest with the agent; the path unit's `PathChanged` on a file written in place |
| The ACL of the cluster CA file and the API's certificate name | The installer copies `/etc/pve/pve-root-ca.pem` and sets `tls_server_name` to the host name; if the certificate does not carry it `doctor` shows a certificate error and you set `tls_server_name` |
| Every PowerShell script against Hyper-V (`New-VHD -Differencing`, the config disk, `Add-VMNetworkAdapterExtendedAcl` and its weights and stateful rules, `Set-VM`) | No Windows host; scripts are written from the module's documentation and only their protocol is tested |
| The Hyper-V network ACLs' effect on DHCP, DNS and replies | Rules are written to let those through; try a machine on your switch |
| The Windows image build on Hyper-V (`hyperv-iso`, floppy answer file, the plugin version) and the guest-tools silent install | Forecrew's CI has no Windows host; `packer validate` is not run here either |
| `forecrew-start.ps1` reading the config disk (volume label, **the drive letter Windows gives it**, offline disks) and waiting for a file in use | Same |
| **IPv6**: Proxmox's output policy DROP, and Hyper-V's `::/0` deny at weight 1 | Try `ping -6` and an IPv6 website from inside a machine: both must fail |
| **Machine-to-host filtering**: that a machine cannot reach the node or host (Proxmox 8006 and 22, Hyper-V's own addresses), the other machines on the bridge or switch, and the LAN, even with `network_allow: [0.0.0.0/0]` | The rules are written in that order and tested against the fakes; whether the firewalls enforce them as written is for you to try |
| **Removing `ipfilter`**: that a Proxmox machine without it has IPv4 and DHCP, and nothing of the VM firewall's other defaults lets it spoof an address | Done because an empty `ipfilter-net0` set is said to drop all IPv4 (Firewall.pm); try a machine and read its rules |
| **`-EncodedCommand` reading standard input**: that the encoded `secure` script gets its JSON from stdin under `powershell.exe -NonInteractive` | Written from the PowerShell documentation; if it fails, no machine is made and the error says the folders were not closed |
| **`Set-Acl` with the VM accounts**: that the agent can close its folders and that the `NT VIRTUAL MACHINE\<id>` accounts Hyper-V adds to a machine's files pass the check | Seen only as a test of the script text |
| **The `vEthernet (<switch>)` adapter name** of the host on the machines' switch, which is how the switch's network is found | A switch with no adapter on the host has no network to close; check the name in `Get-NetAdapter` |
| **Stateful UDP ACLs**: that `-Stateful` on a UDP rule lets a resolver's answer in, and that `Add-VMNetworkAdapterExtendedAcl` takes it for UDP | Documented for TCP; if DNS fails from a machine, say so. The DHCP reply is allowed by its ports (`-LocalPort 68 -RemotePort 67`) because a broadcast request has no tracked answer; if a machine gets no address, say so
| `build-image` end to end | Tested with a fake Packer |
