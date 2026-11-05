# Councils

A council is a temporary group of roles that answers one question and ends
with a decision brief (`docs/contracts/council-brief.schema.json`). The
facilitator frames the question; members write proposals without seeing
each other, critique each other from distinct priorities and revise while
positions still move; the facilitator writes the brief. The data model is
in `docs/contracts/data-model.md` (councils); the `council` playbook runs
the protocol (playbooks.md, Councils). This page describes the packs that
define the standard councils, the council roles and the jobs they run.

## Council packs

A standard council is `packs/councils/<kind>.yaml`, one per
`domain.CouncilKind` except `adhoc`: `onboarding`, `product`, `technical`,
`test_design` and `incident`.

```yaml
kind: technical
question: How should we build it?
facilitator: facilitator
max_rounds: 2
budget: {tokens: 800000, time: 90m}
members:
  - role: architect
    priority: simplicity
    focus: the design with the fewest moving parts that meets the spec, reusing what the code already has
    runtimes: [claude-code, codex]
  - role: devils-advocate
    priority: dissent
    focus: the case against the design most members will favour, and the alternative they overlook
    runtimes: [codex, claude-code]
  # ...
```

| Key | Required | Meaning |
|-----|----------|---------|
| `kind` | yes | The council kind, equal to the file name without `.yaml`. |
| `question` | yes | What this kind of council answers, in general terms. Each council has its own question; this one tells members what kind of answer is wanted. |
| `facilitator` | yes | The role that frames, checks and synthesizes. Its `output_schema` must be `schemas/council-brief.json`. It is not a member and has no vote. |
| `max_rounds` | yes | Revision rounds the council runs on its own while the facilitator says positions still move, from 0 to 2 (`domain.MaxCouncilRevisions`). A human's request for changes adds one more. |
| `budget.tokens`, `budget.time` | yes | The council's token and wall-time budget, both positive. When either runs out the facilitator synthesizes from what exists and sets `budget_exhausted`. |
| `members` | yes, ≥ 3 | The seats. |
| `members[].role` | yes | A role from `packs/roles`, not the facilitator. |
| `members[].priority` | yes | What the seat argues from, e.g. `risk`, `simplicity`, `user-value`, `cost`, `dissent`; `^[a-z][a-z0-9-]*$`, unique within the council. |
| `members[].focus` | yes | What the priority means on this council, one sentence; the member's prompt quotes it. |
| `members[].runtimes` | no | Runtime adapters (`claude-code`, `codex`, `opencode`) the seat prefers, in order. Empty means the role's runtime in forecrew.yaml. |

`councils.Load` decodes strictly (unknown keys are errors), checks every
pack against the role definitions (`roles.Catalog.Definition`) and reports
every problem of every file at once, wrapping `councils.ErrInvalidPack`. A
pack must also have:

- exactly one `devils-advocate` seat;
- at least two different runtimes among the members' first `runtimes`, so
  members run on more than one vendor when the workers offer them;
- a role that sits twice (one role on several runtimes, which
  `council_contributions` allows) with `runtimes` on both seats and none in
  common, so the two seats never run on the same runtime.

The standard councils follow the design doc, with the roles Forecrew has
today: the environment engineer stands in for DevOps, and the test
architect and developer for QA.

| Council | Members (priority) | Rounds | Budget |
|---------|--------------------|--------|--------|
| `onboarding` | architect (simplicity), environment-engineer (operability), test-architect (risk), devils-advocate (dissent) | 1 | 400k tokens, 45m |
| `product` | product-owner (scope), market-researcher (market-fit), ux (user-value), devils-advocate (dissent) | 2 | 600k tokens, 60m |
| `technical` | architect (simplicity), developer (cost), security-reviewer (risk), environment-engineer (operability), devils-advocate (dissent) | 2 | 800k tokens, 90m |
| `test_design` | test-architect (risk), environment-engineer (cost), developer (simplicity), devils-advocate (dissent) | 1 | 400k tokens, 45m |
| `incident` | developer (simplicity), environment-engineer (risk), test-architect (prevention), devils-advocate (dissent) | 1 | 400k tokens, 30m |

Every council has the `facilitator`. Members alternate their preferred
runtimes, so neighbouring seats start on different vendors.

An `adhoc` council has no pack of its own and follows the `technical` one.
`start_council` may name `roles` instead of a kind's members
(`councils.Set.Seated`): a role keeps its seat when the pack has one and
otherwise gets a seat with its name as priority; the devil's advocate sits
whether it is named or not, and a council has at least three members.

When a council starts, each member is pinned to the first runtime it prefers
that an online worker of the role's pool offers and forecrew.yaml has a model
for, or its first one when none does, so its jobs wait for such a worker
(`Pack.Assign`). If that leaves everyone on one runtime while another is
offered to a seat, the last such seat moves there, so two vendors take part
whenever the workers offer them. The facilitator runs on its role's runtime.

## Steps

Each council job runs one step. The prompts are Go templates in
`packs/councils/prompts/<step>.md` (shared parts in `common.md`); the
schemas are in `packs/schemas/`.

| Step | Run by | Output schema | Rounds | Contribution |
|------|--------|---------------|--------|--------------|
| `frame` | facilitator | `council-frame.json`: question, constraints, must_contain, context (evidence), assumptions, escalation | 1 | no |
| `proposal` | each member | `council-proposal.json`: priority, position, recommendation, options (≥ 2), evidence (≥ 1), assumptions, risks, revision (null), escalation | 1 | yes |
| `critique` | each member | `council-critique.json`: priority, per other member's proposal (by `member`) agree, disagree and challenged assumptions with evidence, assumptions, escalation | 1 | yes |
| `check` | facilitator | `council-check.json`: positions_moving, reason, open_disagreements | 1 to `max_rounds`; check n decides whether revision round n runs | no |
| `revision` | each member | `council-proposal.json` with `revision` set: changed, reason, prompted_by (members) | 1 to 3 (the third is the human's) | yes |
| `synthesis` | facilitator | `council-brief.json` | 1 to 2 (the second follows the human's round) | yes |

### Members

Outputs and `council.read` name a member as `role@runtime`, its role and
the runtime adapter its jobs run on, e.g. `architect@claude-code`
(`councils.MemberName`, `common.json#/$defs/member`). The role alone is not
enough, since one role may sit on several runtimes; the pair is what
`council_contributions` keys a member by. The form is used by a critique's
`member`, a revision's `prompted_by`, and the brief's `votes[].member` and
`dissent[].member`.

The devil's advocate dissents because its seat requires it. Its dissent in
the brief starts with `Devil's advocate (mandated dissent): `
(`councils.DevilsAdvocateDissent`), and no other member's does, so a human
can tell it from a real split.

### Jobs

`councils.Set.Job(turn, role)` turns the resolved role of a seat into the
job of a turn (kind, step, round, seat, and for a synthesis whether the
budget ran out):

- the instructions are the step's prompt: the council, the seat with its
  priority and focus, the other seats, the step's rules, the evidence
  rules and the untrusted-data rule;
- the output schema is the step's;
- for a member step, the role's `Runtime` is the runtime the member runs
  on; it must be one of the seat's `runtimes` when the seat lists any, and
  the prompt names the seat as `role@runtime`;
- the job is read-only: no write paths and no write tools
  (`roles.Role.WithoutWrites`), whatever the role usually writes;
- a role written for councils (its `output_schema` is
  `council-proposal.json` or `council-brief.json`) keeps its own prompt,
  placed first in the instructions, and its definition of done and
  escalation conditions (the facilitator's definition of done is about the
  synthesis, so its frame and check jobs drop it). Any other role (architect, developer, ...) sits on
  the council through its seat's priority and focus only: the checklists of
  its usual job are dropped.

Every council job also gets `council.read`, whatever its role lists; a role's
ordinary jobs do not. The pool stays the role's. `contextpack.Render` then adds the Job section
and the output schema as for any job. The runner adds a `Council` section
with the question, the constraints and, from the proposal stage on, the
facilitator's framing; members read the contributions their stage may see
with `council.read` (job-mcp.md), which also returns the constraints as
they stand.

## Evidence

Every claim in a proposal, critique or framing is an evidence item
(`packs/schemas/common.json#/$defs/evidence`): `claim`, `kind` (`path`,
`kb`, `url`, `job`, as `domain.SourceKind`), `source`, `date` and
`confidence`. `councils.Set.CheckOutput(step, output)` checks an output
against its step's schema and what the schema cannot say, wrapping
`councils.ErrInvalid`:

- a `path` or `kb` source is relative to the repo root, optionally with
  `:line` or `:start-end`, without `..`;
- a `url` source is an absolute http(s) URL with its `date` (YYYY-MM-DD);
  other kinds have `date` null;
- a `job` source is a job ref (`JOB-12`);
- in a proposal or revision, option names are unique and the
  recommendation is one of them; `revision` is null in a proposal and set
  in a revision, and a revision's `prompted_by` is empty when `changed`
  is false (a change may come from a human's feedback, so it may also be
  empty when `changed` is true);
- every member is named `role@runtime`;
- a synthesis goes through `councils.ParseBrief`, and only the devil's
  advocate's dissent starts with `Devil's advocate (mandated dissent): `.

After each contribution the action `council.check_evidence` checks every
source once and stores the result as the contribution's `evidence_check`,
which `council.read` shows the members and the facilitator:

| Kind | Check | Outcome |
|------|-------|---------|
| `path`, `kb` | The file exists in the repo at the council's commit, the head of the default branch when it started; lines are ignored. | `ok`, `missing` (a directory counts as missing), `unreachable` when GitHub could not be read |
| `job` | The job exists in the project. | `ok`, `missing` |
| `url` | forecrewd fetches it (`councils.Fetcher`) and archives it as an artifact of the citing job, named `council/url-<hash>`. | `ok`, `unreachable` (no answer, an error status, too slow or too large), `refused` |

- A URL is fetched once per council. A contribution gets at most 10 new URLs
  fetched and a council at most 40; the rest are `unreachable`.
- Only plain http(s) URLs on public addresses are fetched: no credentials in
  the URL, no proxy, at most 3 redirects, 5 MiB and 15 s. The host must resolve to
  public addresses only, at the start and at every redirect, and the connection
  is checked again as it is made, so a name that changes address reaches nothing
  private. Anything else is `refused` without connecting.
- Without evidence storage, or when it fails, a URL that answered is `ok` with a
  `detail` saying it was not archived.

## Council roles

| Role | Tier | Pool | Tools | Prompt |
|------|------|------|-------|--------|
| `facilitator` | best | control | repo.read, git.read | Neutral chair: frames, checks, synthesizes; no option and no vote. |
| `devils-advocate` | strong | control | repo.read, git.read | Makes the strongest honest case against the option the council leans to, in good faith and with evidence. |
| `market-researcher` | strong | control | repo.read, git.read | Every market claim a `url` with its date and a confidence; web search where the runtime has it. |
| `security-reviewer` | strong | build | repo.read, git.read, run.tests | Trust boundaries and exposure; runs the project's existing scanners, so it needs a build worker. |
| `ux` | strong | control | repo.read, git.read | User value: what each option changes for the people who use the product. |

All are read-only, and their council jobs also get `council.read`. Their prompts describe the seat and leave the step's
rules to the step prompt.
