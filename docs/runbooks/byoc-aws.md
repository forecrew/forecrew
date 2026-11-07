# Your own AWS account

For the cloud team of an organization that wants Forecrew's machines in its own
AWS account: they start in your VPC, you pay AWS directly, your code never leaves
the account, and Forecrew holds no credentials for it. Read this before you
deploy anything; it says what the kit creates, what it can and cannot do, and
what it costs you when something goes wrong.

On a hosted Forecrew an own cloud needs the paid plan of your organization
(`fleet.hosted`, [fleet.md](../fleet.md#aws-locations)); a self-hosted instance
has no such gate.

## What you get

```
 forecrewd (ours or yours)            your AWS account
 ┌────────────────────┐   HTTPS out   ┌──────────────────────────────────────────┐
 │ queue, shapes,     │◀──────────────│ fleet agent (one small instance)         │
 │ the plan           │               │   instance role: create, tag, terminate  │
 └────────────────────┘               │   only instances tagged forecrew:fleet   │
                                      │          │ RunInstances                  │
                                      │          ▼                               │
                                      │ one instance per job, in your subnets,   │
                                      │ no AWS credentials, no inbound traffic   │
                                      └──────────────────────────────────────────┘
```

The agent dials out to forecrewd. forecrewd never connects in and never sees an
AWS credential: the agent uses its instance role, found by the AWS SDK the way
every AWS program finds it. What the agent reports to forecrewd are facts: the
account ID, the region, the instance types, checks. The join code is the only
secret you put in the kit.

## Before you start

- A VPC with a subnet for the agent and one to three subnets for machines
  (usually one per availability zone). Machines need outbound internet: a public
  subnet with `AssignPublicIp: true`, or a private subnet behind a NAT gateway.
- A signed AMI of the Forecrew Linux image in the account, or one you will copy
  in (below). The agent boots only AMIs of **your own account**.
- The public key your images are signed with
  (`forecrew image keygen --out image.key --public image.pub`).
- Quotas: the vCPU quota of the instance types you list (EC2 Service Quotas);
  a request over it is `VcpuLimitExceeded`, which the agent reports as no room.
- For spot, once per account:
  `aws iam create-service-linked-role --aws-service-name spot.amazonaws.com`.

## Deploy

Draw a join code that lasts a week, for the team that runs the kit:

```sh
forecrew provider join --cloud
```

Then use one of the two templates in [`deploy/byoc/aws`](../../deploy/byoc/aws);
they make the same things.

**CloudFormation** (console, or the CLI; the join code is `NoEcho`):

```sh
read -rs JOIN && aws cloudformation deploy --stack-name forecrew --template-file cloudformation.yaml \
  --capabilities CAPABILITY_IAM --parameter-overrides \
  ServerUrl=https://forecrew.example.com JoinCode="$JOIN" ImageSigningKey="$(cat image.pub)" \
  FleetName=forecrew-prod-eu1 VpcId=vpc-... AgentSubnetId=subnet-... MachineSubnet1=subnet-... InstallRef=v0.2.0
```

**OpenTofu or Terraform**:

```sh
read -rs TF_VAR_join_code && export TF_VAR_join_code
tofu init && tofu apply \
  -var server_url=https://forecrew.example.com -var "image_signing_key=$(cat image.pub)" \
  -var install_ref=v0.2.0 -var vpc_id=vpc-... -var agent_subnet_id=subnet-... -var 'machine_subnet_ids=["subnet-..."]'
```

`InstallRef` (`install_ref`) is required and must be a release tag, never a branch:
the instance runs that revision's `deploy/fleet-agent/install.sh` as root at first
boot, so read it first.

| Input | Default | Meaning |
|-------|---------|---------|
| `Mode` | `ec2` | `ec2`, `microvm` or `mac` (below) |
| `FleetName` | none (OpenTofu: `forecrew-` and a hash of account, region and VPC) | The `forecrew:fleet` tag value. The role allows exactly this, **and the agent terminates every instance of the account and region that carries it**, whoever started it. It must be unique per account and region: never reuse a name another kit, a script or a person tags with. Two kits with different names never touch each other's machines |
| `InstanceTypes` | four `c7i` sizes | What machines may use; `*` works. The role refuses any other type |
| `AssignPublicIp` | `false` | `true` for a public subnet |
| `MaxMachines` | `10` | The most machines at once, whatever Forecrew asks for |
| `MonthlyBudget` | `0` | Stops new machines at this estimated spend; 0 is none |
| `MaxLifetimeHours` | `24` | No machine lives longer |
| `CopyAmiId`, `CopyAmiRegion`, `CopyAmiDigest` | empty | Copy an AMI into the account (below) |

### Connect

The instance starts the agent, which reports what it found and waits for you:

```sh
forecrew provider joins
forecrew provider confirm <id> --name own-aws --facts <FACTS>
```

**Check the report before you confirm**: the cloud account must be yours, and the
region and the instance types the ones you chose. `forecrew-fleet-agent doctor`
on the agent's instance (through SSM Session Manager or SSH) says what is wrong
when it is not online; its checks (`credentials`, `instance_types`, `subnets`,
`security_group`, `budget`) also show on the location's page.

### The AMI

The signature of an image covers its manifest, not an AMI's ID. What ties your
signed manifest to a real AMI is where the AMI lives: the agent boots an AMI only
if it belongs to **this account** and carries the tag
`forecrew:image-digest` with exactly the digest the image is signed with. An AMI
anyone else owns, public ones included, is refused.

That makes the tag a trust decision: **anyone in your account who may tag AMIs
(`ec2:CreateTags`) vouches for images**. They can put the tag on an AMI of their own
making, and the agent will boot it as the signed image. The copy step below does
not check that the AMI's content matches the digest: it only tags what it was
given, so give it the digest from a manifest you verified and the AMI ID you
built. Keep tagging of AMIs to the people who build images.

To bring an image built elsewhere, give `CopyAmiId`, `CopyAmiRegion` and
`CopyAmiDigest`: the stack copies the AMI into the account (`CopyImage`), tags
the copy and its snapshots, and outputs its ID. Wait until its state is
`available`, then register the image with the URI `ami:<region>/<that id>`
(`forecrew image add`, [images.md](../images.md)). The copy's role can create tags only while copying, and can deregister only what
the kit itself tagged; it is not removed after the copy, because deleting it
would delete the copied AMI with the stack (the custom resource owns it). If you do
not want the role to stay, make the copy once with OpenTofu's `aws_ami_copy` or
`aws ec2 copy-image` instead, and leave `CopyAmiId` empty. Without the copy, tag your own
AMI yourself: `aws ec2 create-tags --resources ami-... --tags
Key=forecrew:image-digest,Value=sha256:...`. The copy is deleted with the stack.

The role of the kit allows launching images and snapshots of the account only. To
trust another account's AMIs, add it to `trusted_ami_owners` in `aws.yaml` and
widen the `ec2:Owner` condition of the role's `RunInstanceImage` statement to
match: both are your decision, and neither is the default.

### Shapes

A shape (machine type) names the instance type. The note is shown on the machine
type, so say what is slow or minimal:

| Shape | `provider_config` | Note |
|-------|-------------------|------|
| `aws-medium` | `instance_type: c7i.xlarge` | Starts a fresh EC2 instance per job: about a minute to boot, slower than a microVM |
| `aws-medium-spot` | `instance_type: c7i.xlarge`, `spot: true` | As above, at spot price; EC2 can take a machine back, and its job runs again |
| `aws-arm` | `instance_type: m7g.xlarge` | Arm; needs an `arm64` image |

`provider_config` takes `instance_type`, `region` (if given, the agent's),
`subnet` (one of the kit's, default the first), `security_group` (the kit's) and
`spot`; anything else is refused when the machine is created. Set `cpu`, `memory_mb` and `gpu` to what the instance type has or less: the agent refuses a shape that promises more, or
of another architecture. Set `price_per_minute` to what the instance costs you per minute: the budgets, here and
in Forecrew, count with it. `disk_gb` is the size of the encrypted gp3 root volume.

## What the role can do

One role, in `policy.json.tftpl` for OpenTofu and spelled out the same in the
CloudFormation template (a test keeps them equal). Everything it can change
carries a tag condition:

| It can | Only |
|--------|------|
| `ec2:RunInstances` | with the fleet tag on the instance, volume and network interface; an instance type of your list; default tenancy (no dedicated hosts); in your listed subnets and the kit's security group; IMDSv2 required with one hop; **no instance profile** (machines have no AWS credentials); an encrypted gp3 volume of at most 1000 GB; an image and snapshot of this account |
| `ec2:CreateTags` | at launch with the fleet tag; afterwards on instances that already have it, never the `forecrew:fleet`, `forecrew:machine` or `forecrew:price` keys (an explicit deny covers the fleet tag) |
| `ec2:DeleteTags` | the same, and a request that names its tag keys: EC2 treats a request with none as "delete every tag", which the role refuses |
| `ec2:TerminateInstances` | instances with the fleet tag, in this account and region |
| `Describe*` (instances, instance types, images, subnets, security groups) | in this region. EC2 does not offer resource-level control of these: the agent can **read** the account's instances, and it asks only for its own tag |

It cannot stop, modify, delete or read the console or user data of anything, touch
a volume or snapshot, call IAM or pass a role, or act outside the region. The
agent also refuses to destroy an instance without its fleet tag before it asks
EC2, so a mistake of forecrewd's is not a request to AWS. The tests prove the
refusals against a model of IAM's rules
(`internal/fleetagent/aws`: `TestRoleCannotTouchUntaggedResources`).

## What machines can reach

The security group is the only network control the kit sets, and it is weaker than
"the internet and nothing else". Read what it does and does not do:

- No inbound rule at all. Outbound is every public IPv4 address, whatever the
  protocol, with 10/8, 172.16/12, 192.168/16, 100.64/10, 127/8, 169.254/16, 0/8 and
  224/3 left out (a group cannot say "everything but", so the rules list the
  public ranges). A machine cannot open a connection to your VPC, peered or
  on-premise ranges by their private addresses.
- **Security groups do not filter Amazon DNS, Amazon Time Sync or the instance
  metadata service.** The DNS and time rules in the group are only a list of what
  the machines are meant to use. In fact a machine can ask the VPC resolver
  anything: it resolves the names of **private hosted zones and any on-premise
  names your Route 53 Resolver forwards**, which tells a job what exists in your
  network, and DNS is a channel to send data out. If that matters, add Route 53
  Resolver DNS Firewall to the machines' VPC, or give them a VPC of their own with
  no private zones or forwarding.
- Metadata: machines are launched with IMDSv2 required, one hop (a container on
  the machine cannot reach it) and without a role, so it holds only the user data,
  a bootstrap token that is spent in the first seconds.
- **Anything that is reachable at a public address is reachable.** That includes S3
  and DynamoDB through a gateway endpoint in the machines' route table, interface
  endpoints and the like, and any service that trusts your VPC, your endpoint IDs
  or your NAT gateway's address (an allow-list on a database, an S3 bucket policy
  on `aws:SourceVpce` or `aws:SourceIp`, a SaaS that knows your egress IP). A job
  runs untrusted code: such a service treats it as your network.
- Recommended: give machines **dedicated subnets** with their own route tables, with
  **no gateway or interface endpoints** (they reach AWS APIs over the internet, with
  no credentials, which is nothing), a NAT gateway or public addresses only, and
  network ACLs that deny private ranges as a second layer. Nothing in a machine's
  subnet should be an exception in somebody's allow-list.
- If your VPC uses public addresses in its own CIDR, they are reachable too. IPv6
  is not opened.
- `network_allow` of the location does not apply to AWS: a machine has no group of
  its own. A location with `network_allow` set refuses to create machines and says
  so. Change what machines reach by changing the security group.
- In `microvm` and `mac` modes the jobs run on the agent's own instance, and what
  they reach is what **that instance's** group allows: the kit's agent group lets
  out HTTPS (443) to any address, DNS and time, and nothing else, so jobs can reach
  only HTTPS services (everything the same caveats apply to). Those jobs also share
  the host with the agent and its role; the microVM or the Tart VM is the boundary.
- Endpoints and NAT gateways have their own prices.

## The bootstrap token and user data

A machine learns forecrewd's URL and its one-time token from the instance's user
data (cloud-init writes `/run/forecrew/bootstrap.env`, RAM, and `forecrew-start`
reads and deletes it, so the worker spends the token before any job runs). Two
things follow, and both are bounded by the token being spent at once:

- Anyone in your account allowed to read an instance's attributes
  (`ec2:DescribeInstanceAttribute`) can read the user data of a running machine.
  The token is single-use and expires with the boot timeout.
- cloud-init keeps a copy of the user data on the machine's own disk. The
  user data removes it once written (best effort); the disk goes with the
  machine, whose volume is encrypted and deleted on termination.

## Machines on one big host

`Mode=microvm` makes the agent's own instance the host: use a metal instance
type, or an instance type with nested virtualization enabled. Jobs then run as
microVMs on that one instance ([fleet.md](../fleet.md#microvm-locations)), booting in
seconds, and you pay for the instance whether or not a job runs. There is no
EC2 provider in this mode and the role has no EC2 permissions. Confirm the report
as a `microvm` location, and give its shapes a rootfs image (the AMI format does
not apply). Check the current AWS list of instance types that support nested
virtualization or are metal before you choose; it changes.

## EC2 Mac

`Mode=mac` allocates a **dedicated host** (`AWS::EC2::Host`) and runs the agent on
a Mac instance on it, with the Tart provider: jobs run in macOS VMs on that host,
at most two at once (Apple's licence). A dedicated Mac host is billed for at least
24 hours from its allocation, whether or not it runs a job, and cannot be
released before that: deleting the stack within the first day fails until the 24
hours pass. Say so in the shape's note, and use a macOS AMI that has Tart and the
Forecrew macOS image (`AgentAmiId`). This mode needs the Tart provider of the fleet
agent build.

## Cost control

Four brakes, each independent of the others:

1. **`max_machines`** in `aws.yaml` (kit: `MaxMachines`): the agent starts no more
   at once, counted from EC2 by tag, whatever forecrewd asks. This one bounds the
   bill even when prices are wrong.
2. **`monthly_budget`**: the agent adds up the price of each shape per minute of
   every machine it starts (a ledger in `state_dir`, kept across restarts) and
   starts no new machine once the month's estimate reaches it. It is an estimate of
   what your shapes say a minute costs, not AWS's bill: use AWS Budgets for the
   bill itself. At `budget_warn_percent` (80) the `budget` check of the location
   turns to a warning, and to a failure at 100%. Forecrew has its own limits on top
   (`limits.max_machines`, `limits.monthly_budget` of the location and of each
   shape), which stop new machines the same way.
3. **Lifetimes.** Every machine shuts itself down after `max_lifetime` (a timer
   in its user data; EC2 terminates an instance that shuts itself down), and the
   agent terminates what outlives it, what is past its deadline, and anything
   stopped, at each of its reconciliations (every 5 minutes).
4. **Disks and interfaces** are created to be deleted with the instance.

### Leaked machines

Everything the agent creates has the tags `forecrew:fleet`, `forecrew:machine`,
and `forecrew:org` and `forecrew:job` when known. forecrewd tells the agent which
instances are orphans (no machine, or a finished one), and the agent terminates
them. If the agent is gone for good, find what is left by tag and terminate it:

```sh
aws ec2 describe-instances --region eu-west-1 \
  --filters Name=tag:forecrew:fleet,Values=forecrew Name=instance-state-name,Values=pending,running,stopping,stopped \
  --query 'Reservations[].Instances[].[InstanceId,LaunchTime,Tags[?Key==`forecrew:machine`]|[0].Value]' --output text
aws ec2 terminate-instances --region eu-west-1 --instance-ids i-...
```

The self-shutdown timer ends them anyway within `MaxLifetimeHours`.

## Spot

A spot machine that EC2 takes back disappears from the account. The agent does
not list it; at its next reconciliation (within 5 minutes) forecrewd fails the
machine as gone from the provider, revokes its worker and requeues its job
through its lease, and the log says `a machine is gone from EC2` with EC2's reason
(`Server.SpotInstanceTermination`). No spot capacity, or a spot quota, is "no
room": the job waits and the shape is tried again.

## Remove

```sh
forecrew provider revoke own-aws
aws cloudformation delete-stack --stack-name forecrew   # or: tofu destroy
```

Revoking stops the agent's token; forecrewd requeues the jobs. Delete the stack
only after `forecrew machines` shows nothing at this location, or terminate what
is left by tag as above. A Mac dedicated host cannot be released before 24 hours.

## When something is wrong

| You see | It means |
|---------|----------|
| `UnauthorizedOperation` in the agent's log | The role refused: the request had no fleet tag, an unlisted type, subnet or group, or an image of another account. The message names the action. Check `FleetName` matches `fleet` in `/etc/forecrew-fleet-agent/aws.yaml` |
| `instance type "x" is not in this agent's instance_types` | The shape names a type the kit was not given; deploy again with it, or change the shape |
| `has no forecrew:image-digest tag` | The AMI is not a copy made by the kit, or its tag is not the digest the image is signed with |
| `belongs to account ..., which this agent does not trust` | The AMI is another account's: copy it in |
| `VcpuLimitExceeded`, `InsufficientInstanceCapacity` | Shown as a job waiting; raise the quota or pick other types or subnets |
| `this agent's limit of N machines` / `monthly budget` | The brakes above; the location shows why |
| `network_allow does not apply to an AWS location` | Clear `network_allow` on the location |
| credentials check fails | The agent's instance has no role, or not this one: it needs the kit's instance profile. For a test from your own machine set `AWS_PROFILE` |
