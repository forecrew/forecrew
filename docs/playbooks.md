# Playbooks

A playbook is a declarative workflow in `packs/playbooks/<name>.yaml`; the
file name is the playbook name. The engine in `internal/workflow` loads them
with `workflow.LoadPlaybooks`, which validates every file and names the
offending step on error. Runs start with `Engine.Start(ctx, name, req)`.

```yaml
description: Spec a goal and get it approved.
inputs: [repo, goal_id]          # required inputs, read as ${inputs.<name>}
steps:
  - id: spec
    type: job
    with: {role: product-owner, repo: '${inputs.repo}', goal_id: '${inputs.goal_id}'}
    on_failure: escalate
  - id: review
    type: loop
    max_iterations: 3
    until: steps.approve.output.decision == approved
    on_failure: escalate
    steps:
      - id: revise
        type: job
        with: {role: product-owner, repo: '${inputs.repo}', feedback: '${steps.approve.output.feedback}'}
      - id: approve
        type: gate
        with: {kind: spec_approval, title: 'Spec round ${iteration}', subject_repo: '${inputs.repo}', summary: '${steps.revise.output.summary}'}
```

## Steps

Every step has an `id` (lowercase letters, digits, `-` and `_`; unique in the
whole playbook) and a `type`. Fields that do not belong to the type are
rejected.

| Type | Fields | What it does |
|------|--------|--------------|
| `job` | `with`, `on_failure` | Creates a job through `workflow.JobCreator`, then waits for its `job.completed`. `with` takes the json keys of `workflow.JobRequest` (`role` required; `account_id`, `project_id` are set by the engine; `goal_id` and `task_id` default to the run's; `prompt` adds instructions to the job's prompt and must be literal text written in the playbook: a `${...}` reference in it is refused when the playbook loads, because the prompt is rendered as trusted instructions). Fails when the job fails or is canceled. |
| `action` | `action`, `with`, `on_failure` | Calls the Go function registered under `action` in `workflow.Actions` with the resolved `with`. Its result is the step output; an error fails the step. |
| `gate` | `with`, `on_failure` | Opens a gate and waits for `gate.resolved`. `with` takes the json keys of `gates.OpenRequest`: `kind`, `title`, `subject_repo` (required), `summary`, `subject_number`, `goal_id`, `spec_id`, `task_id`, `job_id`. |
| `wait` | `with`, `timeout`, `on_failure` | Waits for a signal. `with.signal` is the kind and the remaining keys build its key with the `domain` helpers: `checks.completed` (`repo`, `sha`), `pr.merged` / `pr.closed` (`repo`, `number`), `job.completed` (`job_id`), `gate.resolved` (`gate_id`). The first signal stored under the key completes the step, so for a PR either merged or closed does (the output's `signal` says which). On `job.completed` the step follows the job-step rules: it outputs the job's values and fails when the job did not succeed. On `gate.resolved` it outputs the gate's values but does not apply the gate-decision rules below. Unlike job and gate steps, a wait has no lost-signal fallback. `timeout` (Go duration, e.g. `30m`) fails the step after that long; zero waits forever. |
| `foreach` | `items`, `steps` | Runs `steps` once per element of the list `items` (a single `${...}`), in order. Inside, `${item}` and `${index}` (from 0) are set. |
| `loop` | `steps`, `max_iterations`, `until`, `on_failure` | Runs `steps` until `until` holds after a round, at most `max_iterations` rounds. Inside, `${iteration}` (from 1) is set. When the rounds run out, `on_failure` applies. |

A step waiting for a signal first looks at signals already stored under its
key, so a signal that arrived before the step started is not missed. Stored
signals nobody waits for are kept.

## Values and references

Step values:

- every step: `steps.<id>.output` (job: the job's structured output; action:
  the function's result; gate: `{decision, feedback}`; wait: the signal
  payload for `checks.completed`, `pr.merged` and `pr.closed`, the job's
  structured output for `job.completed`, `{decision, feedback}` for
  `gate.resolved`; foreach: a list with one `{<body step id>: value}` per item;
  loop: `{iterations}`);
- job steps also `steps.<id>.job_id` and `steps.<id>.head_sha`; gate steps
  `steps.<id>.gate_id`; wait steps on `checks.completed`, `pr.merged` or
  `pr.closed` `steps.<id>.signal`. A wait on `job.completed` has the values
  of a job step (`job_id`, `head_sha`) and a wait on `gate.resolved` those of
  a gate step (`gate_id`).

References are `${inputs.<name>}`, `${steps.<id>.<field>[.<field>...]}`,
`${item...}`, `${index}` and `${iteration}`. Numeric fields index lists
(`${steps.spec.output.tasks.0.title}`). There is no expression language.

- A string that is exactly one `${...}` takes the referenced value as is: a
  list, an object or a number.
- Otherwise references are interpolated as text; lists and objects as JSON.
- A reference to a step that has not run yet (e.g. the review feedback in
  round 1 of a loop) is empty. References are checked on load: inputs must
  be declared and step ids must exist.
- Inside a loop, a round sees the step values of the previous round until
  it overwrites them, and after the loop the last round's values stay
  visible. A foreach body's values are only visible inside that item.

`until` is `<ref>`, `<ref> == <value>` or `<ref> != <value>`; the ref may be
bare or in `${...}` and the value may be quoted. Values compare as text. A
bare ref holds when it is set and not `false`, empty or zero.

## Gate decisions

- `approved`: the step succeeds and the run continues.
- `rejected`: the run is canceled.
- `changes_requested`: inside a loop the step succeeds and the run
  continues, so the next round can read `${steps.<gate>.output.feedback}`;
  end the loop with `until: steps.<gate>.output.decision == approved`.
  Outside a loop it fails the step, so `on_failure` applies.

A resolved gate is final; each round or retry opens a new gate.

## Failures and escalation

`on_failure` is `fail` (default; the run fails) or `escalate`. Escalate opens
a `needs_you` gate on the project's repo:

- approve retries the step as a new attempt (for a loop: one more round);
  the approval's feedback is added to the `feedback` of a retried job (for a
  loop that ran out of rounds it is not passed on: the extra round only sees
  the values of the previous round, such as the last gate's feedback);
- reject cancels the run.

`foreach` has no `on_failure`; a failing body step decides with its own.

### `finally`

A playbook may end with a top-level `finally:` list of `action` steps (no
`on_failure`; they can reference `${inputs.*}` only, since step values are
not kept for them):

```yaml
finally:
  - {id: cleanup, type: action, action: onboarding.settle}
```

The engine runs them once, in order, when the run ends `failed` or
`canceled`, before the run gets that status; a run that succeeds skips them.
They are recorded as step runs `finally/<id>` like any step, so after a crash
the recorded ones are not repeated and the rest continue (an action in
flight at the crash is called again with the same `Key`, like any action). A
failing one is logged and the next still runs. Use them to undo state the run
changed, since a failed or canceled run stops where it is.

## Durability and idempotency

Each step run is recorded before its side effect and marked done after it,
and the side effect is keyed by the step run ID, so a run resumed after a
crash continues where it stopped:

- **Jobs**: the engine sets `JobRequest.ID` to the step run ID and the
  `JobCreator` must create the job with that ID (the step fails otherwise).
  On resume the engine finds the job instead of creating another.
- **Gates**: opened with the step run ID; opening it again returns the gate.
- **Actions** run at least once: a crash between the call and the step being
  recorded calls the action again with the same `ActionCall.Key`. Actions
  must be idempotent for their key (for example by checking whether the
  issue, comment or PR they create already exists).

If a job or gate step's job or gate finishes but its signal is lost, the
engine reads the job or gate state when it next looks at the run. Wait steps
have no such fallback: they finish only on a stored signal (or time out). Runs are woken at once by signals
published in the same process and otherwise by the periodic sweep (30 s).

## Actions (`internal/playbooks`)

`playbooks.Register(registry, deps)` adds these to the action registry:

| Action | `with` | Output |
|--------|--------|--------|
| `project.register` | `repo` | `{project_id, repo, default_branch}`. Fails unless the GitHub App is installed on the repo; moves the run's project registered → onboarding (already onboarding is fine). |
| `github.open_or_update_pr` | `repo`, `title`, `body`, `head`, `base`, optional `previous` | `{number, url, head_sha}`. Updates the open PR from `head` or opens one, and upserts its `changes` row. With `previous`, the number of an earlier round's PR, it fails when a human closed that PR instead of opening another, and when a human merged it outputs `merged: true` with `merge_sha` and changes nothing. `if_open` is `[number]`, or `[]` for a merged PR, for a `foreach` that skips steps. |
| `github.merge_pr` | `repo`, `number`, optional `check_profile` | `{merged, already_merged, sha}` (the merge commit). Squash-merges unless already merged. Only a head whose checks succeeded (or has none) is merged; it fails on failed or running checks, and with `check_profile` when the profile file at the head is invalid. Use `on_failure: escalate`. |
| `profile.validate` | `profile` and/or `repo` and `ref`, `attempt`, `attempts` | `{valid, errors}`: checks the profile value and/or the profile file of `repo` at `ref`. Fails on the last attempt. |
| `profile.feedback` | `human`, `errors` | The feedback text of an analyst job. |
| `onboarding.proposal` | `profile`, `summary`, `assumptions` | `{title, body}`: the profile PR text. |
| `onboarding.settle` | none | Puts a project still onboarding back to registered. |
| `onboarding.complete` | `repo`, `sha`, `ref` | Reads `.forecrew/profile.yaml` at the merge commit `sha`, or at `ref` when `sha` is empty (GitHub merged the PR), validates and stores it, and moves the project to onboarded. |

## The onboarding playbook

`packs/playbooks/onboarding.yaml` takes `repo` and `project`. An
`onboarding-analyst` job writes `.forecrew/profile.yaml` on
`agent/onboarding`; its profile is validated (one rerun with the errors, then
the run fails; the profile file the analyst committed is validated too), a PR
is opened or updated (a PR closed or merged on GitHub in between fails the
run), and the playbook waits up to an hour for `checks.completed` on the
analyst's commit (the webhook and poller report `none` for a commit without
checks; a timeout opens a `needs_you` gate). Then a `profile_approval` gate is
opened on the PR. Approval squash-merges the PR unless GitHub merged it
already; failing or running checks, or an invalid profile file (a human may
have edited it), open a `needs_you` gate instead, and approving that retries.
The profile file as merged is then stored, read at the merge commit, so edits
made in the PR count. A PR a human merges on GitHub counts as the approval,
also between two rounds (the approval step is skipped and the run goes on to
store the merged file); a PR a human closes fails the run. A failure while
storing the profile opens a `needs_you` gate and never undoes the onboarding
by itself. Rejecting that gate is an explicit human stop: the project goes
back to registered, and the merged profile file stays on the default branch. Changes
requested start a new round with the feedback, at most three before a
`needs_you` gate. A rejection, a closed PR or any failure ends the run, and
its `finally` step puts the project back to registered.
