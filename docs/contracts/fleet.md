# Fleet

The fleet starts a machine for a job and throws it away. The worker does not
change: every machine runs `forecrew-worker`, and what is new is the layer
that creates and destroys machines from queue demand. Static workers keep
working next to it, with no machine row.

Tables are in data-model.md, the API of the fleet agent in
fleet-agent-api.md, the worker's first call in worker-api.md
(`POST /worker/v1/bootstrap`). Types are in `internal/domain/fleet.go`, the
provider interface in `internal/fleet`, the resource schemas in
`fleet.schema.json`. T57 fixes the contracts; the tasks named below build them.

## Four ideas

| Idea | Means | People see | Table |
|------|-------|------------|-------|
| Pool | Trust zone: which roles and credentials (config.md `pools`) | Trust zone, under More options | config |
| Shape | Kind of machine: OS, arch, size, price, where it comes from | Machine type | `shapes` |
| Capacity provider | Where machines come from | Location | `capacity_providers` |
| Image | What is inside: OS, worker, agent CLIs, toolchains; versioned and signed | Software | `images` |

A *machine* is what runs a job, usually for minutes. YAML and the API keep the
technical names; the UI says location and machine type.

## Ownership

`account_id` NULL on a provider, shape or image makes it **instance-wide**:
offered to every organization whose policy allows it, and managed only by
instance admins. Everything else belongs to one organization.

- A shape or join code has the organization of its provider: both the same
  one, or both instance-wide (triggers `shapes_check` and
  `provider_join_codes_check`). Neither a provider nor a shape ever changes
  organization.
- Machines of an organization's provider are `own`; machines of an
  instance-wide provider are `hosted` (`domain.Ownership`). Usage records keep
  which it was.
- `forecrew provider add` without `--org` uses the server's organization when
  it has only one, so a home server is the organization's own. On a server with
  several `--org` is required: a slug, or `instance` for an instance-wide
  provider, so nobody makes one by accident.

Every amount (`price_per_minute`, budgets, `usage_records.cost`) is in the one
currency of the instance, config.md `fleet.currency`. A price of 0 is free.

## Shapes

| Field | Meaning |
|-------|---------|
| `name` | `domain.ValidName`; unique per organization, and among instance-wide shapes. |
| `provider_id` | The capacity provider; in resource YAML, `provider` by name. |
| `os` | `linux`, `windows`, `macos`. |
| `arch` | `amd64`, `arm64`. |
| `cpu`, `memory_mb`, `disk_gb` | Size of a machine; positive. |
| `gpu` | Number of GPUs. Which GPU is a label (`gpu:l4`). |
| `pool` | Pool of the workers on its machines, default `build`, never `actions`. |
| `labels` | What the machines offer, as for a worker: `docker`, `runtime:claude-code`, `xcode:16`. List what the image's `contents.labels` says. The OS and arch labels are added (`Shape.OfferedLabels`) and are what the worker gets. |
| `provider_config` | What the provider needs: instance type, template, region, subnet (below). JSON, no secrets. |
| `image` | Image name, or `name@version` to pin one (`domain.ParseImageRef`). Unpinned means the newest built (`ImageRepo.Resolve`). |
| `price_per_minute` | What a minute costs; own machines at home are 0. |
| `monthly_budget` | Stops new machines of this shape for the month; NULL means no limit. |
| `warm_min` | Machines kept up with no job: `ready`, or warm (`Machine.Warm`). |
| `max` | Machines of this shape at once, ≥ 1 and ≥ `warm_min`. |
| `idle_seconds` | How long a machine up above `warm_min` waits for a job; 0 destroys it at once. Idle time counts from the latest of `ready_at`, `assigned_at` and the end of its last job, so an assigned warm machine is not destroyed before its first job. |
| `boot_timeout_seconds` | How long a machine may take from entering `booting` until its worker calls, before it is failed and replaced; default 300. Also how long a bound bootstrap token lasts. |
| `max_jobs` | Jobs a machine takes in its life, counted by `machines.jobs_taken`, and its worker's `max_jobs`; default 1. |
| `note` | What a person should know, shown on the machine type: slower boot on plain EC2, the 24-hour minimum on EC2 Mac. |
| `enabled` | A disabled shape gets no new machines. |

`Shape.Validate` checks a shape alone. `Shape.CheckProvider` checks it against
the capabilities its provider reported: OS, arch, size, GPUs and `warm_min`
against the provider's machines; a provider with no capabilities yet accepts
no shape. `Shape.CheckImage` checks that the image fits the reference, OS and
arch and that the provider starts its format.

**No secrets.** `provider_config` holds settings, never credentials: those stay
where the machines are (fleet-agent-api.md). `Shape.Validate` refuses, at any
depth, a key that looks like a secret (`*token*`, `*secret*`, `*password*`,
`*credential*`, `*private_key*`, `*api_key*`, `*access_key*`) unless its value
is a reference (`env:NAME` or `secret:name`; not `store:name`, the secret
store of forecrewd), which the fleet agent resolves on its own host, and any value shaped like a credential
(`domain.SecretPatterns`). `fleet.schema.json` says the first half too.

Keys of `provider_config` by kind; each provider owns its keys and refuses
unknown ones in `doctor`:

| Kind | Keys |
|------|------|
| `microvm` | `vmm` (`firecracker` default, `cloud-hypervisor`) |
| `proxmox` | `node`, `storage`, `template`, `bridge` |
| `hyperv` | `base_disk`, `switch` |
| `tart` | `base_vm` |
| `aws` | `instance_type`, `region`, `subnet`, `security_group`, `spot` (bool) |

## Capacity providers

| Field | Meaning |
|-------|---------|
| `name` | `domain.ValidName`; unique per organization, and among instance-wide providers. |
| `kind` | `microvm`, `proxmox`, `hyperv`, `tart`, `aws`; `fake` for tests, which nothing in production serves. A new kind is a value here and a migration. |
| `mode` | `agent`: a fleet agent dials out and holds the credentials. `direct`: forecrewd calls `fleet.Provider` itself; used by the fake provider, and later by delegated cloud roles. |
| `status` | `pending`: no token yet. `active`. `paused`: finishes running machines and starts no new ones. `revoked`: its token no longer works. |
| `token_hash` | SHA-256 of the provider token (`fcp_`); agent mode only. |
| `last_seen_at` | Last contact. Online means seen in the last 60 s (`CapacityProvider.Online`), like a worker. |
| `capabilities` | What it reported, below. |
| `limits` | `{max_machines, monthly_budget}`, absent means no limit. |
| `network_allow` | Hosts or CIDRs its machines may reach beyond the default (Network). |
| `tags` | Copied to everything it creates; no secrets. |

`capabilities` (`domain.ProviderCapabilities`): `os`, `arch`, `max_cpu`,
`max_memory_mb`, `max_disk_gb`, `max_gpu` (per machine), `max_machines` (at
once on this host, like Apple's two VMs per Mac; the controller never asks for
more), `image_formats`, `checks` [{`name`, `status` `ok` | `warn` | `fail`,
`detail`}], and the facts a person confirms: `host`, `cloud_account`, `region`,
`agent_version`. A provider may run `CapacityProvider.MaxMachines()` machines,
the lower of `limits.max_machines` and `capabilities.max_machines`.

### fleet.Provider

Go interface in `internal/fleet`, implemented by the fleet agent for each
kind (T59, T61–T64), by `internal/testkit`'s fake (T58), and by forecrewd in
`direct` mode:

```go
type Provider interface {
	Capabilities(ctx context.Context) (domain.ProviderCapabilities, error)
	Create(ctx context.Context, req CreateRequest) (Instance, error)
	Update(ctx context.Context, externalID string, req UpdateRequest) error
	Destroy(ctx context.Context, externalID string) error
	List(ctx context.Context) ([]Instance, error)
}
```

- **Capabilities** reports what is true now; it may change between calls.
- **Create** gets the machine's ID, shape, resolved image, bootstrap, tags,
  network allowlist and deadline, and returns when the machine is booting. It
  is idempotent for the machine ID, so a retry after a crash never makes a
  second one: a machine with that ID that already exists is destroyed and
  started again, because a retry carries a new bootstrap token. No room is
  `fleet.ErrCapacity`, which does not fail the machine.
- **Update** applies new `tags`, `network_allow` and `deadline` to a running
  machine without restarting it. A warm machine gets its organization's tags and
  network when it is assigned, and every machine a deadline when it leases a job.
  Each field is the whole new value, and applying the same values again succeeds.
- **Destroy** removes the machine with every disk, network device and jail, or
  resets it where the provider reuses a VM (Hyper-V checkpoints). Destroying
  what does not exist succeeds. Either way the machine's row is finished: a
  reset VM comes back as a new machine with a new ID.
- **List** returns everything of Forecrew's the provider holds, found by tag,
  and nothing else. It is how orphans are found.
- The bootstrap token reaches the guest by the provider's own means (cloud-init,
  a config disk, a shared directory), is never written to the host's disk
  outside the guest's own image, and is never logged (`fleet.Bootstrap` hides
  it).
- Everything created carries `forecrew:machine`, and `forecrew:org` (the
  organization's ID, which unlike its slug never changes) and `forecrew:job` when
  they are known, as tags or the provider's closest thing,
  next to the provider's `tags`.

## Machines

```
requested → booting → ready → busy → draining → destroyed
    │          │        │       │        │          ▲
    └──────────┴────────┴───────┴────────┴─→ failed ┘
requested → destroyed            booting, ready → draining
```

| State | Means | Set by |
|-------|-------|--------|
| `requested` | The row exists; the provider may not know it yet. The controller writes it first, so a restart finds what it asked for. | forecrewd |
| `booting` | The provider started it; `external_id` is known and `booting_at` set. A warm machine waits here. | provider report |
| `ready` | It has a worker: its bootstrap token was exchanged. | forecrewd, at `POST /worker/v1/bootstrap` |
| `busy` | It leased a job. It stays busy until `jobs_taken` reaches the shape's `max_jobs` and its last job ends, or until it sits idle for `idle_seconds`. | forecrewd, at lease |
| `draining` | It takes no more jobs and is being destroyed. | forecrewd |
| `destroyed` | Gone; `destroyed_at` is set. | provider report |
| `failed` | It broke; `error` says why. It may still exist at the provider, and goes to `destroyed` once it is gone. | either |

`domain.MachineState.Transition` is the table. `busy` never goes back to
`ready`: a machine does not return to the warm pool after a job. `Counts` says
which states count against limits. A failed machine's job is requeued by its
lease, as for any worker, so a job is never lost.

**Organization.** A machine of an organization's shape belongs to it from the
start. A warm machine of an instance-wide shape has none (`account_id` NULL,
**unassigned**) until demand assigns it (`assigned_at`), and that assignment
never changes: trigger `machines_check` enforces it, the composite keys keep
its worker and job in that organization, and its worker must be ephemeral.

**Warm.** A machine with no organization cannot have a worker, because
`workers.account_id` is required. So it stays `booting`: its worker's first call
gets `202` and sets `ready_at`, which is what makes it warm (`Machine.Warm`),
counts it for `warm_min` and stops its boot timeout. It reaches `ready` after
assignment, like any machine, when the waiting worker exchanges its token. Its
idle time starts again at `assigned_at`: the latest of `ready_at`, `assigned_at`
and the end of its last job counts, so time spent warm is not held against it and
it is not destroyed before its first job.

**Audit.** Every create and destroy goes to the organization's audit log. An
unassigned machine has no organization and `audit_log.account_id` is required,
so what happens to it, and to an orphan with no known organization, reaches only
the server log.

**Boot timeout.** It starts at `booting_at`, when the provider has started the
machine, and ends at `ready_at` (`Machine.BootExpired`). A machine kept
`requested`, because its provider is offline or has no room, is not timing out:
the controller may withdraw it, but does not fail it.

**Worker.** Every machine is a worker with `ephemeral` true, made at bootstrap
with the shape's pool, `max_jobs` and offered labels. It is revoked when its
machine starts draining. Static workers have `ephemeral` false and no machine
row, and nothing here changes them. A worker leases only jobs of its
organization, as `Jobs.LeaseNext` always did; so an organization's machines
lease only its jobs.

**Leasing.** An ephemeral worker also leases only jobs whose `resources` fit
its machine's shape (`Shape.Fits`), and only while `jobs_taken` is below the
shape's `max_jobs`. `Machine.CanTakeJob` is an early look and two concurrent polls
can both pass it, so the rule is `MachineRepo.TakeJob`: one conditional update in
the lease's transaction that adds one `WHERE jobs_taken < max_jobs`, and answers
`ErrConflict` to the loser, whose lease is given up. `machines_check` refuses
`jobs_taken` rising past `max_jobs`, so no code path can go over it; a machine
already past a `max_jobs` lowered later can still drain and be destroyed. T58 adds
this to the leasing query (data-model.md). A static worker reports no size, so
`resources` do not restrict it: labels route jobs to it. `job_id` is the job the
machine last leased. A machine started for a job is not reserved for it: the
first job that fits takes it, and demand is counted again.

**Deadline.** `deadline_at` is set at lease time to the job's timeout plus the
lease duration, and the machine is destroyed then whatever it is doing. NULL
while it has no job.

### Bootstrap tokens

`bootstrap_tokens` (`domain.BootstrapToken`), prefix `fcb_`, one per machine,
hash only:

- bound to the machine and exactly its organization (trigger
  `bootstrap_tokens_check`), and to an expiry of the shape's boot timeout. The
  expiry counts from assignment, or from the delivery for a machine that was
  assigned before it: a warm machine may wait for days, then gets the whole
  timeout;
- exchanged once for a worker token, at `POST /worker/v1/bootstrap`, only from
  `booting`. A machine that is `ready`, `busy`, `draining`, `destroyed` or
  `failed` gets `401`, so a copied token cannot bring a dead machine back;
- minted when it is delivered to the provider, because only the hash is kept.
  It is delivered, and so replaced, only for a machine that is `requested` and
  that the agent does not list as having (fleet-agent-api.md, poll), so a slow
  `Create` keeps its token. A failed `Create` is reported `failed`; when that
  report is lost the machine is delivered again, which is why `Provider.Create`
  restarts a machine that exists;
- on an unassigned machine, without organization and expiry: such a token only
  gets `202`, which never moves the machine out of `requested` or `booting`.
  Assigning sets both, in one transaction with the machine's, and the waiting
  worker gets its token. It can do nothing before that, so it needs no expiry;
  the machine's life is the controller's to end.

## Job resources

`jobs.resources` (`domain.Resources`): `min_cpu`, `min_memory_mb`,
`min_disk_gb`, `gpu`, each absent or 0 for no minimum. An agent job takes them
from `resources:` in its role pack (roles.md), a command job from `resources:`
in its environment (environment.schema.json; not for provider `actions`,
whose runners have a fixed size). They are set when the job is created.

## Picking a shape

`domain.SelectShapes(shapes, policy, demand)`, with
`ShapeRepo.ListAvailable` and `Job.ShapeDemand`. A shape **matches** a job when
it is:

1. of the organization, or instance-wide (never another organization's);
2. allowed by the policy: routing and `allowed_shapes`;
3. enabled;
4. of the job's pool;
5. offering every label the job needs (`Job.RequiredLabels`: `needs` and, for
   agent jobs, `runtime:<runtime>`);
6. at least as big as `resources` asks.

Matching shapes are ranked: under `own_first` own shapes before hosted ones;
then the lowest `price_per_minute`; then the smallest (fewest GPUs, CPUs,
memory, disk); then by name. There is no other priority: because the cheapest
match wins, free machines at home are used before paid ones.

The controller takes the first shape in that order with room. Room is not part
of the ranking: a shape has none when its provider cannot start machines
(`CapacityProvider.CanStart`) or a limit is reached (shape `max` or budget,
provider limits, organization quotas). With no room anywhere the job waits with
a reason; with no match at all it waits too, and the reason says what is
missing. A job never fails for want of a machine. No machine is created for a
job an idle online worker of its pool can lease.

### Worked examples

The shapes, for organization A:

| Shape | Owner | OS | Size | Labels | Price |
|-------|-------|----|------|--------|-------|
| `home-small` | A | linux | 2 CPU, 4 GB | docker | 0 |
| `home-medium` | A | linux | 4 CPU, 8 GB | docker | 0 |
| `home-windows` | A | windows | 4 CPU, 8 GB | | 0 |
| `home-off` | A, disabled | linux | 1 CPU, 1 GB | | 0 |
| `home-control` | A, pool `control` | linux | 1 CPU, 2 GB | | 0 |
| `aws-large` | A | linux | 8 CPU, 16 GB | docker | 0.04 |
| `aws-gpu` | A | linux | 8 CPU, 32 GB, 1 GPU | docker | 0.21 |
| `cloud-small` | instance | linux | 2 CPU, 4 GB | docker | 0.01 |
| `cloud-xl` | instance | linux | 16 CPU, 64 GB | docker | 0.08 |
| `other-free` | B | linux | 64 CPU, 256 GB | docker | 0 |

All have `runtime:claude-code` and are in pool `build` unless the table says
otherwise. Each example is a job of that runtime in `build`, and each is a case
of `TestSelectShapes`:

| # | Routing | Job asks | Ranked result |
|---|---------|----------|---------------|
| 1 | `own_only` | `docker` | `home-small`, `home-medium`, `aws-large`, `aws-gpu` |
| 2 | `own_only` | 4 CPU, 8 GB | `home-medium`, `home-windows`, `aws-large`, `aws-gpu` |
| 3 | `own_only` | 64 GB | nothing: the job waits, no machine type |
| 4 | `own_first` | 64 GB | `cloud-xl` |
| 5 | `own_first` | `docker` | `home-small`, `home-medium`, `aws-large`, `aws-gpu`, `cloud-small`, `cloud-xl`: own first, though `cloud-small` costs less than `aws-large` |
| 6 | `hosted` | `docker` | `cloud-small`, `cloud-xl` |
| 7 | `own_only` | 1 GPU | `aws-gpu` |
| 8 | `own_only` | `windows` | `home-windows` |
| 9 | `own_only` | pool `control` | `home-control` |
| 10 | `own_first`, allowed `home-medium`, `cloud-small` | `docker` | `home-medium`, `cloud-small` |

## Fleet policy

`fleet_policy`, one row per organization (`domain.FleetPolicy`). An
organization without a row has `domain.DefaultFleetPolicy`: `own_only`, so code
stays on its own machines until someone decides otherwise. That default would
leave an organization of a hosted instance with no machines at all, so hosted
organizations need a `fleet_policy` row set when they are created, with the
routing chosen on the first-run screen (T65, T66).

| Field | Meaning |
|-------|---------|
| `routing` | `own_only`: only its own shapes. `own_first`: own shapes, then hosted ones when none has room. `hosted`: only instance-wide shapes. Widening routing lowers protection (T66). |
| `allowed_shapes` | Shape names it may use, own or hosted; empty allows all. |
| `quotas` | `concurrent_machines`, `monthly_minutes`, and `monthly_budget`, which counts hosted machines only; absent means no limit. |
| `egress` | Hosts or CIDRs its hosted machines may reach beyond the defaults (Network). |
| `plan` | `free` or `paid`, a stub until billing (S.3). It is what the instance sells, so no role of the organization changes it, `worker.manage` and `billing.manage` included: in the `fleet_policy` resource a change of `plan` is accepted only from a person flagged as an instance admin acting through their own session (not an OAuth grant), and on a hosted instance (`fleet.hosted`) never. Attaching an own cloud to a hosted organization needs `paid` (T64), and priorities and free-plan limits read it (T65). |

Months are calendar months in UTC. Minutes and budgets count usage records of
the month plus the machines still running.

### Fairness

Organizations share the instance-wide machines. Each pass places queued jobs
one at a time: an organization's oldest first, and between organizations the
next job is of the one with the fewest hosted machines for its plan's weight
(`paid` 3, `free` 1), then of a paid one, then the oldest job. Two busy
organizations split a hosted shape's `max` between them, a paid one three to one
against a free one, and neither takes it all by queueing first. Own machines
are the organization's alone and do not count.

An organization on the `free` plan is also held to `fleet.free_plan`
(config.md): hosted machines at once and hosted minutes a month, both off by
default. A job that waits for one says so in `job_status`, as any quota does:
`machine_limit` "The free plan runs 2 hosted machines at once", `budget` "The
free plan's 600 minutes of hosted machines this month are used up".

A hosted machine takes one job, whatever the shape's `max_jobs` says
(`fleet.JobsPerMachine`): the worker gets `max_jobs` 1, and a lease past it is
refused in the lease's transaction. Each job of a paying organization gets a
fresh machine.

## Abuse

A location may watch its machines for abuse (`fleet.AbuseChecker`; the microVM
provider's `abuse` keys, docs/fleet.md): CPU held full for long, alone or while
connecting to mining pools. It stops such a machine at once and its agent
reports it `failed` with `abuse` (fleet-agent-api.md). forecrewd then fails the
machine, cancels its job rather than requeue it, writes `machine.abuse` to the
organization's audit log and opens a **notice** (`fleet_notices`,
`domain.FleetNotice`), one per machine and kind, for the instance's operator.
Instance admins list notices with `list_fleet_notices` (`GET
/api/v1/fleet/notices`, `forecrew notices`) and close them with
`resolve_fleet_notice`. A notice's `detail` is the agent's words: untrusted.

Free-plan limits are in Fairness.

## Network

By default a machine reaches the internet and nothing else: not the host, the
LAN, other machines or metadata endpoints. `capacity_providers.network_allow`
adds hosts or CIDRs for a home or on-premise location (T61). On hosted
machines egress is narrower, model APIs, GitHub and package registries, and
`fleet_policy.egress` adds to it (T65). A desired machine's `network_allow` is
the sum of what applies.

On a hosted instance (`fleet.hosted`) an instance-wide location is of kind
`microvm` (`fleet.HostedKinds`, `fleet.RequireHostedKind`): adding or confirming
one of another kind is refused, and the controller starts no machine on one, the
job waiting with the reason. An AWS location, an organization's own cloud,
refuses a hosted machine itself.

On a hosted machine that sum is `fleet.HostedEgress` (model APIs, GitHub, the
usual package registries), the hosts of the organization's enabled model
connections to custom endpoints, and its `egress`, with the location's own
`network_allow` first; a warm machine, which has no organization, gets
`fleet.HostedEgress` alone, and the rest with its organization on assignment
(`Provider.Update`). The location keeps a hosted machine to that list
(microVM `egress: allowlist`, docs/fleet.md): a name that does not resolve
opens nothing, and no entry opens a private or link-local network, the host, a
network of the host or another machine, whatever an organization writes. Every
machine has its own network (a tap and a table of rules of its own, no bridge),
so machines of one organization are apart from each other as much as from
another's.

Known limit: a machine always reaches its resolver on port 53, so data can
leave a machine inside DNS queries (DNS tunnelling) whatever its egress list
says; the list is not a guarantee that nothing leaves. A location closes that
only with a resolver of its own that answers the allowed names alone.

## Images

`images` (`domain.Image`): `name`, `version`, `os`, `arch`, `format` (`rootfs`,
`qcow2`, `vhdx`, `ami`, `tart`), `uri` (where to fetch it, without credentials, with a scheme of `https`, `http` (both with a host), `s3`, `gs`, `oci` or `ami`, and no credential-looking key in its query, such as `X-Amz-Signature`;
an AMI is `ami:<region>/<id>`), `digest` (`sha256:<hex>` of the image file; for
an AMI or a Tart image, which are not one file, of the build's manifest),
`signature`,
`contents`, `built_at`. `account_id` NULL is an image of the instance; an
organization registers its own builds under its ID. The manifest of a build is
`$defs/image` of fleet.schema.json.

- `signature` is `ed25519:<base64>` over `Image.SigningPayload` (name, version,
  os, arch, format, digest, the kernel's digest on a `rootfs` image and the
  build time to the second, one per line), so a signature fits one image only
  and neither the kernel nor the build date can be changed under it. The fleet
  agent checks it, and the digests of the image file and of the kernel file,
  against the public keys it trusts before first use, and refuses the machine
  otherwise.
- **Who signs.** Forecrew's images are signed in the release pipeline with the
  project's key, and an instance's own with its operator's; agents trust the
  matching public keys. An organization's images are signed by a person of that
  organization with its own key, kept off the host, and its public key goes in the
  local config of its own fleet agents, not through forecrewd. A fleet agent holds
  no signing key and never signs anything.
- **Registering.** `POST /api/v1/images` (`forecrew image add`, T60) with a
  person's credentials: `worker.manage` for the organization's images, instance
  admin for instance-wide ones. There is no route for an agent, and
  `Image.Validate` requires a signature everywhere, so an unsigned image cannot be
  stored. The server checks the form only; the agent checks the signature.
- **Built on a host.** A Windows image from a user's ISO (T60, T62) is built by
  the fleet agent, which leaves an unsigned manifest on that host. A person
  checks it, signs it, and registers it under the organization. So a host agent
  can never register an image others trust: the row needs a person, it is only
  the organization's, and only agents configured with its key accept it. The plan
  of that step says so.
- `contents`: `labels` (a shape on the image lists them), `packages` [{`name`,
  `version`}] (the Software line) and, on `rootfs` only, `kernel` {`uri`,
  `digest`}.

## Usage

`usage_records` (`domain.UsageRecord`), one per assigned machine, written when
it is destroyed (T65): organization, machine, shape, `ownership`, `minutes`,
`cost`, `started_at`, `ended_at`. A composite key ties the organization and shape
to the machine's own, so a machine's cost cannot land on another organization. A machine is metered from the later of
`assigned_at` and `booting_at` to `destroyed_at` (`fleet.MeterStart`), so the
time a warm machine waits costs nobody, and a machine kept `requested` by a
location with no room costs nothing (one that never started has a record of 0
minutes); `cost` is `minutes` × the shape's `price_per_minute` then, minutes to
the thousandth and cost to the millionth, as the columns keep them. An
unassigned machine has no record. `UsageRepo.Create` refuses a second record
for a machine, so metering can be retried.

The record is written in the transaction that ends the machine: the one that
moves it to `failed` or, if it never failed, to `destroyed`. A failed machine is
billed to its failure (`fleet.MeterEnd`), also when nothing ever reports it gone,
as when its location is revoked, and its later destruction writes nothing more.
So a machine never ends without a record and never gets two; the
`machine.failed` or `machine.destroyed` audit entry carries its `minutes` and
`cost`. Once an hour the controller also meters any ended machine (failed or
destroyed) left without a record
(`UsageRepo.Unmetered`), with a `machine.metered` audit entry, so a record that
was missed is late, never lost and never silent. Budgets and quotas count the
month the same way (`MeterStart`). `fleet_usage` (`GET /api/v1/usage`, `forecrew
usage`) reports an organization's month by shape and ownership, the input of
billing.

## Permissions

`worker.read` reads machines, providers, shapes, images and usage;
`worker.manage`, shown as Manage machines, changes providers, shapes and the
fleet policy, except their budget fields, which need `billing.manage`
(permissions.md, Budget fields). Instance-wide rows are for instance admins.
