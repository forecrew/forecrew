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
| `job` | `with`, `on_failure` | Creates a job through `workflow.JobCreator`, then waits for its `job.completed`. `with` takes the json keys of `workflow.JobRequest` (`role` required; `account_id`, `project_id` are set by the engine; `goal_id` and `task_id` default to the run's; `pool` picks the worker pool and defaults to the role pack's `pool`, and a pool whose policy does not run the role fails the step (docs/worker.md, Pools); `task` makes the job a read-only task of the role instead of its usual work (`packs/tasks/<role>.<task>.md` with its output schema `<role>.<task>.json`; the knowledge curator has `retro` and `curate`, the advisor `revise`); `prompt` adds instructions to the job's prompt and must be literal text written in the playbook: a `${...}` reference in it is refused when the playbook loads, because the prompt is rendered as trusted instructions; `locked_paths`, a list of globs, goes into the job spec and the prompt: the job fails if it changes a matching file, see Locked tests; `write_paths`, a list of globs, is added to the role's write paths for this job, so a read-only role writes the files they name (the architect's ADR in `goal-v3`): take it from an action, never from a job's output, and run the job in a pool that gets a write token). Fails when the job fails or is canceled. |
| `action` | `action`, `with`, `on_failure` | Calls the Go function registered under `action` in `workflow.Actions` with the resolved `with`. Its result is the step output; an error fails the step. |
| `gate` | `with`, `on_failure` | Opens a gate and waits for `gate.resolved`. `with` takes the json keys of `gates.OpenRequest`: `kind`, `title`, `subject_repo` (required), `summary`, `subject_number`, `goal_id`, `spec_id`, `task_id`, `job_id`. |
| `wait` | `with`, `timeout`, `on_failure` | Waits for a signal. `with.signal` is the kind and the remaining keys build its key with the `domain` helpers: `checks.completed` (`repo`, `sha`), `pr.merged` / `pr.closed` (`repo`, `number`), `job.completed` (`job_id`), `gate.resolved` (`gate_id`), `council.decided` (`council_id`). The first signal stored under the key completes the step, so for a PR either merged or closed does (the output's `signal` says which). On `job.completed` the step follows the job-step rules: it outputs the job's values and fails when the job did not succeed. On `gate.resolved` it outputs the gate's values but does not apply the gate-decision rules below. A wait for `gate.resolved` (like a gate step) finds the gate resolved when the signal was lost; the other waits have no lost-signal fallback. `timeout` (Go duration, e.g. `30m`) fails the step after that long; zero waits forever. |
| `foreach` | `items`, `parallel`, `steps` | Runs `steps` once per element of the list `items` (a single `${...}`), in order, or with `parallel: <n>` at most n items at a time (see [Parallel foreach](#parallel-foreach)). Inside, `${item}` and `${index}` (from 0) are set. |
| `loop` | `steps`, `max_iterations`, `until`, `on_failure` | Runs `steps` until `until` holds after a round, at most `max_iterations` rounds. Inside, `${iteration}` (from 1) is set. When the rounds run out, `on_failure` applies (`fail`, `escalate` or `skip`, see [Failures and escalation](#failures-and-escalation)). |

A step waiting for a signal first looks at signals already stored under its
key, so a signal that arrived before the step started is not missed. Stored
signals nobody waits for are kept.

## Values and references

Step values:

- every step: `steps.<id>.output` (job: the job's structured output; action:
  the function's result; gate: `{decision, feedback}`; wait: the signal
  payload for `checks.completed`, `pr.merged` and `pr.closed`, the job's
  structured output for `job.completed`, `{decision, feedback}` for
  `gate.resolved`, `{council_id, status, decision_id}` for `council.decided`;
  foreach: a list with one `{<body step id>: value}` per item;
  loop: `{iterations}` (and `skipped` for a loop with `on_failure: skip`));
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

## Skipping steps

The engine has no `if`. To run steps only sometimes, put them in a `foreach`
over a list that is empty when they must be skipped, and let an action output
that list: `github.open_or_update_pr` outputs `if_open` (`[number]` while the
PR is open, `[]` once it is merged), `goal.assess` outputs `if_ready` and
`goal.if_approved` outputs `if_approved`.

```yaml
- id: gated
  type: foreach
  items: '${steps.pr.output.if_open}'   # skipped when the PR was merged
  steps:
    - {id: checks, type: wait, with: {signal: checks.completed, repo: '${inputs.repo}', sha: '${steps.pr.output.head_sha}'}}
```

A foreach outputs a list with one `{<body step id>: value}` per item, so the
values of a skipped body are not there: a later step or a loop's `until`
reads them as `steps.gated.output.0.checks.output.conclusion`, which is empty
when nothing ran. Inside the foreach `${item}` is the list's element and hides
an outer foreach's, so read what an outer item held from a step output
instead.

## Parallel foreach

`parallel: <n>` runs up to n items at once; 0 or absent runs them one after
the other. It must be a number, is checked on load and fits `foreach` only.

```yaml
- id: reviews
  type: foreach
  parallel: 3
  items: '${steps.plan.output.tasks}'
  steps:
    - {id: review, type: job, with: {role: reviewer, repo: '${inputs.repo}', branch: '${item.branch}'}}
```

- Items start in order, and an item is in flight from its first step until its
  last one finishes, also while it waits for a gate or a signal. When an item
  finishes, the next one starts.
- The output is the same list as without `parallel`, in item order, however
  the items finish.
- The step runs are keyed by item index as always, and what is in flight
  follows from them, so after a restart each item continues where it stopped.
- The foreach finishes when every item has finished. An item whose step fails
  (`on_failure: fail`) does not stop the others: the items not started yet
  still run, and the run fails afterwards with the failure of the first such
  item. So `parallel: 1` differs from no `parallel`, which stops at the first
  failure. An item that escalates waits for its gate while the others go on.
- A canceled run (a gate or an escalation rejected in any item) stops at once:
  no other item starts, every job in flight is canceled and its step fails
  with `canceled: the run ended`, and then the `finally` steps run. A gate
  still open in another item stays pending; deciding it has no effect.
- Parallelism is that of jobs, gates and waits. One run advances on one engine
  worker, so the actions of a body are called one at a time and a slow action
  delays the items after it.

## Failures and escalation

`on_failure` is `fail` (default; the run fails) or `escalate`. Escalate opens
a `needs_you` gate on the project's repo:

- approve retries the step as a new attempt (for a loop: one more round);
  the approval's feedback is added to the `feedback` of a retried job (for a
  loop that ran out of rounds it is not passed on: the extra round only sees
  the values of the previous round, such as the last gate's feedback);
- reject cancels the run.

A `loop` also takes `on_failure: skip`, for a part of a playbook that must not
fail the run (the learn step of `goal-v3`). A failure inside the loop (a failed
step or the rounds running out) opens a `needs_you` gate: approve ends the loop
as skipped and the run goes on, reject cancels the run. The loop's output has
`skipped` (`true` or `false`); the steps inside keep the default `fail`, since
`escalate` would retry them first.

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
changed, since a failed or canceled run stops where it is. An action reads how
the run is ending from `ActionCall.Run.Status`.

## Durability and idempotency

Each step run is recorded before its side effect and marked done after it,
and the side effect is keyed by the step run ID, so a run resumed after a
crash continues where it stopped:

- **Jobs**: the engine sets `JobRequest.ID` to the step run ID and the
  `JobCreator` must create the job with that ID (the step fails otherwise).
  On resume the engine finds the job instead of creating another.
- **Gates**: opened with the step run ID; opening it again returns the gate.
- **Items of a parallel foreach** resume the same way, each from its own step
  runs.
- **Actions** run at least once: a crash between the call and the step being
  recorded calls the action again with the same `ActionCall.Key`. Actions
  must be idempotent for their key (for example by checking whether the
  issue, comment or PR they create already exists).

If a job or gate step's job or gate finishes but its signal is lost, the
engine reads the job or gate state when it next looks at the run, and so does
a wait for `gate.resolved`. Other wait steps have no such fallback: they finish
only on a stored signal (or time out). Runs are woken at once by signals
published in the same process and otherwise by the periodic sweep (30 s).

## Actions (`internal/playbooks`)

`playbooks.Register(registry, deps)` adds these to the action registry:

| Action | `with` | Output |
|--------|--------|--------|
| `project.register` | `repo`, optional `keep` | `{project_id, repo, default_branch}`. Fails unless the GitHub App is installed on the repo; moves the run's project registered → onboarding (already onboarding is fine). With `keep` the status is left alone and an onboarding or onboarded project is accepted (the test part of onboarding). |
| `github.open_or_update_pr` | `repo`, `title`, `body`, `head`, `base`, optional `previous`, `task_id` (defaults to the run's task) | `{number, url, head_sha}`. Updates the open PR from `head` or opens one, upserts its `changes` row and adds it (`owner/name#number`) as a dependent of the assumptions the task rests on. With `previous`, the number of an earlier round's PR, it fails when a human closed that PR instead of opening another, and when a human merged it outputs `merged: true` with `merge_sha` and changes nothing. `if_open` is `[number]`, or `[]` for a merged PR, for a `foreach` that skips steps. |
| `github.merge_pr` | `repo`, `number`, optional `check_profile`, `sha`, `require_verification` | `{merged, already_merged, sha}` (the merge commit). Squash-merges unless already merged. Only a head whose checks succeeded (or has none) is merged; it fails on failed or running checks, with `check_profile` when the profile file at the head is invalid, with `sha`, the head that was approved, when the head moved away from it, and with `require_verification` unless the stored verification report of the head being merged has the status `passed` (no report, for example after a push that came after it, `failed` and `incomplete` are all refused, with the blocking reasons in the error). The report's status decides, never the check run `forecrew/verification`: an incomplete report is a neutral check run, which the checks above count as passed. Use `on_failure: escalate`. |
| `actions.install_workflow` | `repo`, `branch` | `{path, changed}`. Writes the `forecrew-matrix` workflow (`packs/workflows`) to `.github/workflows/forecrew-matrix.yml` on `branch`, which command jobs of pool `actions` dispatch (actions.md). `changed` is false when the file is already identical. Needs the App's Contents and Workflows write permissions. Fails when forecrewd has no GitHub Actions support configured. The `onboarding-tests` playbook runs it. |
| `goal.intake` | `repo`, `goal_id` | `{issue_number, issue_url, default_branch}`. Creates the labels `forecrew:goal` and `forecrew:task` if missing and opens the goal's issue, labeled `forecrew:goal`, unless the goal has one. For a goal of kind `bug` it also creates `forecrew:bug`, puts it on the issue and adds it to an issue the goal has already. A goal from a GitHub ticket in the project's repo has that issue, and the goal of any other ticket links it in its body. |
| `bug.spec` | `goal_id` | `{spec_id, revision}`. Stores the report of a bug goal as its spec (revision 1, one criterion, `AC-1`: the failure no longer happens, shown by a regression test) and moves the goal to spec review. A repeated call returns the same spec. Refuses a goal that is not a bug. |
| `bug.plan` | `goal_id`, `spec_id`, `platforms` | `{valid, errors, tasks}`, like `goal.plan`, with the one task `Fix GOAL-n` (the title of the report is not in task titles) for `AC-1` and its issue under the goal's; moves the goal to planned. |
| `bug.brief` | `problem`, `note` | `{feedback}`. The feedback of the next test engineer attempt: the problem `bug.reproduced` found and the note a person gave at the gate, trimmed and separated by a blank line; empty in the first attempt. |
| `bug.reproduced` | `ready`, `feedback`, `tests`, `base`, `missing` | `{done, feedback, if_stuck}`. `ready`, `feedback` and `tests` are from `tests.map`; `base` is the output of `tests.base_check_result` and `missing` the one of `tests.base_check_request`, both empty when the tests were not ready and the base check did not run. Test ids in the text are quoted as untrusted data. `done` is true when the test engineer's tests are ready, there is one and it failed on the default branch. Else `feedback` says why (no test, `cannot_reproduce`, a test that passes on base, a base check no worker could run) and `if_stuck` is `[feedback]`, for a `foreach` that opens a gate. |
| `goal.size` | `goal_id`, `triage` | `{councils, if_product, if_technical}`. A goal without a size takes it from `triage`, the `triage` job's output: the councils the sizing rule gives (config.md, sizing) are stored as `goals.size`. A goal that has a size keeps it and `triage` is not read: a human's override (create_goal with `councils`) is such a size, so that goal skips the triage job. `if_product` and `if_technical` are `[goal_id]` when the goal gets that council, else `[]`, for a `foreach` that skips the council steps. Refuses a triage with an unknown grade, no area or no reason. |
| `goal.untriaged` | `goal_id` | `{if_triage}`. `[goal_id]` for a goal without a size, else `[]`, for a `foreach` that runs the triage job only when no human chose the councils. |
| `goal.council_constraints` | `goal_id` | `{constraints}`. What a council of the goal starts with, binding on every option: the platforms it targets (`The change must work on: ios, web.`), and nothing else. The goal's title and text are the human's request, not rules; members read the goal in their pack, marked as data. `[]` for a goal without platforms. |
| `goal.adr` | `goal_id` | `{write_paths, pool, branch}`. For the architect's job in `goal-v3`: when a technical council of the goal decided and the project's knowledge policy lets the job write its ADR, `["docs/adr/**"]`, the `build` pool and the branch `agent/adr-GOAL-n`; otherwise `[]` and two empty strings, which leaves the job read-only in its usual pool. The policy lets it when no protected section holds `docs/adr/` and no section reaches beyond it (a section of `docs/adr/**` that is not protected is opened for this job, see `PathRules.DeniedFor`). Read-only. |
| `goal.adr_ready` | `goal_id` | `{if_adr, sha, title, body}`. `if_adr` is `[branch]` when `agent/adr-GOAL-n` exists, the architect having pushed an ADR, with the head `sha` and the text of its PR, else `[]`. |
| `goal.adr_round` | `decision`, optional `feedback` | `{if_rework, done}`. How a round of the ADR's review ended, from the decision of its gate (empty when the architect wrote no ADR). `done` is true for no gate and for `approved`; `changes_requested` is not done and `if_rework` is `[feedback]` (a default text when the human wrote none), for a `foreach` that reruns the architect. |
| `goal.spec` | `goal_id`, `job_id`, `spec` | `{spec_id, revision, summary}`. Stores the product owner's output as the next spec revision (once per job), adds the spec as a dependent of the assumptions the job stated, renders it under the goal's text in the goal issue (in a comment, once, when that issue is a person's ticket, whose text stays: tracker.md), moves the goal to `spec_review`. `summary` is the text of the `spec_approval` gate. When the goal has a decided product council, the summary also names its brief (`get_council COUNCIL-n`) and what it recommended. |
| `goal.plan` | `goal_id`, `spec_id`, `platforms`, `plan`, `spec` | `{valid, errors, tasks: [{id, ref, number, seq, title, issue_number, branch, needs, criteria}]}`. Stores the architect's tasks in order, one issue per task (labels `forecrew:task` and `needs:<label>`, a sub-issue of the goal's), comments the plan on the goal issue and moves the goal to `planned`. `needs` are the task's labels plus `platforms`. A plan that cannot be used (the architect escalated, no task, an unknown criterion, a dependency that is not an earlier task, criteria of the spec that no task covers) stores nothing: the output is `valid: false` with `errors` as the architect's feedback, which is also commented on the goal issue (once per call) for the human a `needs_you` gate asks. Kept tasks and issues are not created twice. |
| `goal.round` | `task_id`, `iteration`, `human`, `review` | `{task_id, ref, title, branch, feedback}`. Before a developer job: the task goes (back) to `in_progress`, the goal to `in_progress`, the branch `agent/TASK-n` is recorded. `feedback` is `human` in the first round and `review` in the others. |
| `goal.task_move` | `task_id`, `to` | `{status}`. Moves a task to a state, through the ones in between when going forward, or back to `in_progress`. |
| `goal.pr_text` | `task_id`, `result` | `{ready, title, body, if_escalated}`: the PR text from the developer's output, the task with its criteria, the assumptions (the register's for the task, with refs and status, else the developer's) and `Closes #<task issue>`. When the developer reports an escalation it comments the reason on the task's issue and outputs `ready: false`, with `if_escalated` holding the escalation quoted as untrusted data (else `[]`), for a `foreach` that opens a gate showing it. |
| `goal.review_post` | `repo`, `number`, `review` | `{posted}`. Posts the reviewer's output as a `COMMENT` review (never an approval or a request for changes): comments on a line are inline, the rest goes into the body with the verdict and the criteria check. Posts once per call key. |
| `goal.assess` | `repo`, `number`, `checks`, `review`, `result`, `sha`, optional `task_id`, `job_id`, `require_verification` | `{done, merged, if_ready, feedback, gate_summary, sha}`. Looks at the PR again: merged on GitHub is `done` and `merged`; closed there fails. Failing checks or a verdict other than `approve` give the rework `feedback`. With `task_id` it also reads the assumption register: a high-impact assumption the task rests on that is still `unverified`, or one invalidated after the developer job `job_id` finished, is not `done` and its `feedback` names the assumptions (statements as untrusted data). Otherwise `done`, `if_ready: [number]`, the `merge_approval` gate's `gate_summary` (checks, verification, verdict, criteria, the task's high-impact assumptions with their status, its open medium-impact ones) and `sha`, the commit the developer pushed that the gate is about, for `github.merge_pr`'s `sha`. With `require_verification` the gate summary also says when a developer job of the task ran without the lock on the tests of the test engineer, and the stored verification report of `sha` decides too: `failed` is rework `feedback` with what blocks (quoted as untrusted data), `passed` adds a `Verification:` line with its summary and the PR link to `gate_summary`, and no report or an `incomplete` one fails the action, because the playbook must finish the verification first. |
| `goal.hold` | `task_id`, `repo`, `number` | `{gates, assumptions, clear}`: the IDs of the pending `assumption_review` gates, and the refs, of the high-impact assumptions the task rests on that are still unverified; `clear` when no review is pending or the PR was merged or closed on GitHub. Read-only. A `loop` until `clear` runs it, with a `foreach` of `wait` steps on `gate.resolved` (`timeout` 72h, `on_failure: escalate`), so the run waits for a human to accept or reject them instead of spending rework rounds, and again after a human asked for changes (that opens a new review). |
| `goal.if_approved` | `decision`, `number` | `{if_approved}`: `[number]` for `approved`, else `[]`. |
| `goal.task_done` | `task_id`, `repo`, `number`, `feedback` | `{done, merged, sha, feedback}`. A merged PR moves the task to `merged` (`done`); otherwise it outputs the human's `feedback` for the next delivery; closed without merging fails. |
| `goal.close` | `goal_id` | `{tasks, cost_usd}`. Needs every task merged. Comments the summary (tasks, PRs, developer runs, cost of every job of the goal) on the goal issue, closes it and moves the goal to `done`. |
| `goal.settle` | `goal_id` | `{status}`. Cancels a goal that is not done and its running councils, closes its unmerged tasks and their issues and the goal's with a comment (once). An issue of the goal that is its ticket stays as it is: the route's `canceled` mapping decides (tracker.md). A task whose PR is merged, as the change or GitHub say, becomes `merged`, not `closed`: merged PRs stay merged. The goal's `per_goal` knowledge proposals that are queued or in review are rejected and the learn PR closed (as `goal.learn_settle` does for a failed run), since the learn step will not run. |
| `tests.map` | `task_id`, optional `needs`, `repo` | `{ready, if_ready, if_architect, feedback, tests, files, suites}`. `if_ready` is `[task_id]` when `ready`, else `[]`, for a `foreach` that runs steps only for tests that are ready. For a task of a bug goal only the latest job counts, because each round of the reproduction starts over. Reads every successful `test-engineer` job of the task (a retry lists only what it changed itself, so all count): `tests` (`suite`, `id`, `file`, without repeats), `files` and `suites` are the union over them, sorted, for `tests.base_check_request` and `matrix.plan`, except that a later job's entries for a test id replace all earlier entries for that id (a retry corrects the map; one job may list a test once per criterion). `verification.build` and `tests.locked_paths` read the same corrected map. `ready` is true when the latest job did not escalate, every `**AC-n**` of the task is in a test or in `untestable` with a reason, and no test is mapped to a criterion the task does not have (such a test would count for nothing; the report also blocks when one fails or did not run). With `repo` it also reads the matrix at the task's branch: one that is invalid or lacks a suite of the tests is not ready. Otherwise `feedback` says why (the escalation is quoted as untrusted data, the missing criteria are named) and `if_architect` is `[{task_id, branch, needs}]` (the task's branch `agent/TASK-n` and the `needs` given, the task's worker labels) when the escalation mentions the matrix or a suite, or the matrix check of `repo` failed, else `[]`, for a `foreach` that runs the `test-architect` first; inside the `foreach`, `${item}` is that element. |
| `tests.locked_paths` | `task_id`, optional `iteration`, `decision`, `feedback`, `base` | `{locked_paths, feedback}`. The files of every successful `test-engineer` job of the task (`files` and the `file` of every test in its `test-map.json` output; a retry lists only what it changed itself, so all jobs count), as globs that match exactly those paths (`[`, `*`, `?`, `{`, `}` become one-character classes), sorted; `[]` when the task has no such job. Fails for a file name with a backslash or starting with `!`. Pass it as `with.locked_paths` of the developer's `job` step; the JobCreator refuses control characters and backticks in it. With `iteration` above 1, the round of the loop around the developer's job after an escalation, and `decision` `approved`, the decision of the gate that asked a human, it is `[]`: that rerun has no lock (see [Locked tests](#locked-tests)). Any other `decision` keeps the lock. The output `feedback` is `base`, the feedback the round already has, followed after a blank line by `feedback`, the human's note of that gate (later rounds only), all trimmed, so the developer's job gets no empty section. |
| `tests.base_check_request` | `repo`, `base`, `head`, `tests`, optional `files`, `only_runnable` | `{requests, runnable, missing}`. `tests` are the `tests` entries of the test map (`suite`, `id`, `file`). Reads `.forecrew/matrix.yaml` and the environment files at `head`. For every suite the tests belong to and every environment its cells run in, one command job request (the first cell that maps to that environment) that runs the suite on `base` with `overlay` = `head` and the files of `files` and of `tests` together. Only test files may be overlaid: a path the `test-engineer` role may not write (`Makefile`, `go.mod`, `.forecrew/**`, scripts) fails the action. With no tests it returns empty lists and `head` may be empty. Fails when `head` has no valid matrix or environment, or names no such suite. Use the requests as the `items` of a `foreach` of `kind: command` job steps. With `only_runnable`, `runnable` leaves out the requests that no online worker of their pool has the labels for (as in `matrix.plan`), so a queued job nobody can lease does not hold the run, and `missing` names them (quoted as untrusted data); a request for a suite and environment that no required cell runs is optional, and without a worker it is dropped from `requests` too and not named, unless no required cell runs the suite at all, which makes its requests needed. Without the option `runnable` is all of `requests`. Run `runnable` and give `requests` to `tests.base_check_result`, which then finds the runs that are missing. |
| `tests.base_check_result` | `runs`, `tests`, optional `requests` | `{new_tests: [{id, failed_on_base}], ok}`. `runs` are the base check's jobs: job IDs, the outputs of job steps (`job_id`), or the rounds of a `foreach` over a job step (`{<step id>: {job_id, ...}}`); an element without a job ID fails the action. `tests` are the same entries as for the request. Reads the test runs the jobs stored. A test failed on base when, in every run of its suite, it has a result and all its results (a repeated id has several) are `failed` or `errored`, or it has no result and the command really failed: `exit_code` above 0 and the run is `errored`, has a `(command)` result, or is not `passed` (a compile error of one package next to an unrelated failing test counts). It does not count when the exit code is 0 (wrong report path, unreadable or empty report), -1 (timeout or not started), or the run is an oversized one (`max_test_results_size`). A test that passed or was skipped did not fail on base. One whose suite has no run at all was not checked: it is left out of `new_tests` and `ok` is false, so the report says it was not checked instead of saying it passed on base. `ok` is true when every new test failed on base. With `requests`, the output of `tests.base_check_request`, the runs are compared with the requests: a suite with fewer stored runs than requests in an environment (a job that succeeded and left no run, or one that was lost) was not checked everywhere, so its tests are left out of `new_tests`, where `verification.build` reports them as not yet checked (`incomplete`), and `ok` is false. |
| `verification.build` | `change`, `sha`, optional `task_id`, optional `base_check` | `{status, blocking, summary, report_id}`. Builds the verification report of the PR `change` (a number in the run's project) at the head `sha` from the stored test runs of `sha` (all attempts; each run's environment, cell and suite are read from the stored run, never from `with`), the matrix at `sha`, the criteria of the task (`task_id`, else the change's task: the `**AC-n**` items under "Acceptance criteria" in its body), the test map of its successful `test-engineer` jobs and `base_check`, the output of `tests.base_check_result`. See [Verification](#verification). It stores the report (one per change and `sha`, rebuilt in place), renders it as an artifact, updates the PR comment (found by its hidden marker) and sets the check run `forecrew/verification` on `sha`. `status` is `passed`, `failed` or `incomplete`; `blocking` lists why the merge is blocked. Fails when `sha` has no valid matrix. Repeating it with the same data changes nothing. |
| `verification.rerun_cells` | `report_id` | `{requests}`. The command job requests that run again every required suite of the report that failed or errored and ran once, at the report's `sha` (no overlay), as the `items` of a `foreach` of `kind: command` job steps. A cell that was rerun already is left out, so a report is rerun once. Build the report again afterwards. |
| `verification.read` | `change`, `sha` | `{status, blocking, summary, complete, if_incomplete, text}`. Reads the stored report of the PR `change` at `sha` and builds nothing. `complete` is false for `incomplete`, and `if_incomplete` is then `[sha]`, else `[]`, for a `foreach` that asks a human. `text` is for a job such as the reviewer: the summary, and what blocks, quoted as untrusted data. Fails when `sha` has no report. |
| `profile.validate` | `profile` and/or `repo` and `ref`, `attempt`, `attempts` | `{valid, errors}`: checks the profile value and/or the profile file of `repo` at `ref`. Fails on the last attempt. |
| `discover.github` | `repo` | `{integrations, notes, text}`. Reads, with the App's token, the repo's autolinks and the link hosts in the bodies of its 100 most recent issues and pull requests, and outputs the integrations found (confidence and evidence as in the profile). It never contacts the hosts it finds. A read the App may not do is a note, not a failure; `text` is what the analyst is told, empty when nothing was found. Read-only, so idempotent. Use `on_failure: escalate`. |
| `matrix.plan` | `repo`, `ref`, `scope`, `suites`, optional `only_runnable`, `strict` | `{cells, jobs, errors, unrunnable, missing}`. Reads `.forecrew/matrix.yaml` and the environments its rules name at `ref` and expands them (see [Running a matrix](#running-a-matrix)). Invalid files, a scope that does not fit the matrix, or a matrix over the plan's limits give `errors` as text and empty `cells` and `jobs`, for the playbook to act on; only a read that fails fails the step. With `only_runnable`, a job that no online worker of its pool has the labels for (its `needs`, those of its environment and `docker` for a docker environment; pool `actions` has no workers and always runs) is moved from `jobs` to `unrunnable` (`{key, pool, needs}`), and `missing` says in text, one line per pool and set of labels, what is missing, quoted as untrusted data since the names come from the repo; both are empty otherwise. A job that no required cell asks for (an optional cell or suite) is optional: without a worker it is dropped from `jobs` and left out of `unrunnable` and `missing`, because nothing waits for it. A suite in `suites` that no required job runs at all (`required: false`, asked for because its tests are new) is the only check of those tests, so its jobs are needed and reported. A queued job nobody can lease would hold its run forever, so a playbook plans with it and asks a human when `missing` is not empty. With `strict`, invalid files fail the step (the error quotes them as untrusted data) instead of being `errors`, so a step with `on_failure: escalate` asks a human. Read-only, so idempotent. |
| `council.run` | `kind`, `question`, optional `constraints`, `roles` | `{council_id, ref}`. Starts a council as a child run, with the run's goal; the council's ID is the call's key, and the run's ID the council's, so a repeated call finds both. Follow it with a `wait` on `council.decided` (see [Councils](#councils)). |
| `council.start` | `council_id`, `roles` | `{facilitator, members: [{role, priority, runtime}], commit}`. First step of the `council` playbook: pins the members to runtimes and reads the head of the default branch, the council's commit. Read-only. |
| `council.contribute` | `council_id`, `step`, `round`, `job_id`, `output`, optional `budget_exhausted` | `{valid, errors, contribution_id, moving, if_gate}`. Checks a council job's output (`councils.Set.CheckOutput`). Invalid gives `valid: false` and `errors` as feedback for a rerun (a check counts as positions not moving). Member stages and the synthesis are stored as contributions with the job's runtime, once per job. A synthesis also becomes the brief, `budget_exhausted` included whatever the facilitator wrote; `if_gate` is `[1]` unless the policy accepts it. |
| `council.check_evidence` | `council_id`, `contribution_id`, `commit` | `{problems}`. Checks every cited source and stores `evidence_check` (see [Councils](#councils)). A checked contribution is left alone. |
| `council.round_done` | `council_id`, `stage`, `round`, `moving`, `members` | `{exhausted, members, if_check, done, round}`. Ends a stage. `stage` is `proposal` (`members` run the critiques), `critique` (`if_check` is `[round]` within the pack's `max_rounds`), `check` (`members` revise when `moving`, otherwise `done`) or `synthesis` (`round` of the next brief). Once the budget ran out `members` and `if_check` are empty. Read-only. |
| `council.open_gate` | `council_id`, `job_id` | `{gate_id}`. Opens the `council_decision` gate with the brief as summary, marked as agent text, and records it on the council in the gate's transaction, so its decision always finds the council. The gate's ID is the call's key. |
| `council.decide` | `council_id`, `gate_id`, `decision`, `members` | `{done, members, round}`. No gate: refuses a `decision` and a recommendation the policy does not accept, and records the policy's decision (`registers.Service.AcceptRecommendation`). A gate must be the council's and have the status `decision` says; an empty or unknown value is refused. `approved`: finds the decision the register stored with the gate. Both move the council to `decided` and publish `council.decided`. `changes_requested`: adds the gate's feedback as a constraint and outputs the human's round. `rejected`: fails the council, publishes `council.decided` and fails the step. |
| `council.settle` | `council_id` | `{status}`. The `finally` step: a running council fails, or is canceled when the run is, and `council.decided` is published. A decided council stays. |
| `profile.feedback` | `human`, `errors`, `discovery` | The feedback text of an analyst job: the reviewer's feedback, the validator's errors and the `discovery` text, each when not empty. |
| `onboarding.proposal` | `profile`, `summary`, `assumptions` | `{title, body}`: the profile PR text. |
| `onboarding.settle` | optional `profile_done` | A project still onboarding goes back to registered, unless its profile was stored (`profile_done`, or the run's step `complete` succeeded): it is then onboarded, without a baseline. Any other status is left alone. |
| `onboarding.complete` | `repo`, `sha`, `ref`, optional `hold` | Reads `.forecrew/profile.yaml` at the merge commit `sha`, or at `ref` when `sha` is empty (GitHub merged the PR), validates and stores it, and moves the project to onboarded; with `hold` it stays onboarding until `onboarding.start_tests` or `onboarding.finish`. |
| `onboarding.start_tests` | `repo`, `project` | `{started, run_id}`. Starts the `onboarding-tests` playbook as a run of its own (the call's key is its ID, so a repeated call finds it). Without entries for `test-architect` and `environment-engineer` under `roles` in forecrew.yaml there is no test part: it moves the project to onboarded and `started` is false. |
| `onboarding.finish` | none | Moves the project from onboarding to onboarded; one that is onboarded stays. |
| `onboarding.tests_feedback` | `human`, `errors` | The feedback text of a test architect or environment engineer job: the reviewer's feedback and the validator's errors, each when not empty. |
| `onboarding.tests_check` | `errors`, `smoke`, `architect`, `engineer` | `{valid, errors}`. `errors` is the output of `matrix.plan`, `smoke` that of `tests.smoke_result`, `architect` and `engineer` are the jobs' `escalation`s; `valid` when none says anything. The loop that runs it escalates when its tries run out. |
| `onboarding.tests_branch` | none | `{branch}`: `agent/onboarding-tests-<12 characters of the run's ID>`, a branch of its own for the run. The branch of an earlier run is merged but not deleted, and a job continues an existing branch, so a refresh would never see that there is nothing to change. |
| `onboarding.tests_head` | `repo`, `head_sha` | `{ref, if_pushed}`. `head_sha` is the environment engineer job's: when the two jobs changed the repo `ref` is that commit and `if_pushed` is `[head_sha]`; when not, `ref` is the head of the default branch and `if_pushed` is `[]`, so a `foreach` over it skips the PR. |
| `onboarding.tests_proposal` | `plan`, `environments`, optional `workflow` | `{title, body}`: the tests PR text from the two jobs' outputs (strategy, axes, matrix changes, environments, assumptions, open questions); a non-empty `workflow` (the `if_actions` of `tests.smoke_plan`) adds a section that names `.github/workflows/forecrew-matrix.yml`. |
| `onboarding.baseline_ref` | `repo`, `merged` | `{sha}`: `merged` (the merge commit of the tests PR) or, when empty, the head of the default branch. |
| `tests.smoke_plan` | `repo`, `ref`, `environments` | `{requests, if_actions}`. `environments` is the environment engineer's list. For every environment it lists as `added` or `changed`, one command job request (items for the `from` of a `kind: command` job step) whose command is the environment's `versions` commands joined with `&&` (`echo smoke` when it has none): the job builds the environment and runs them. The request is that of the first matrix cell that uses the environment, so it lands where the real runs do. `if_actions` is `[repo]` when an environment of the matrix runs on GitHub Actions (the workflow must be installed), else `[]`. A dispatch needs the workflow on the default branch, so while it is not there the environments on `actions` get no smoke job: the baseline runs them once the PR is merged. A matrix that does not load gives no requests: `matrix.plan` reports why. |
| `tests.smoke_result` | `runs` | `{errors}`. `runs` are the smoke jobs, as for `tests.base_check_result`. Text for the roles: every environment whose command (its versions commands) exited with an error, from the test runs the jobs stored; empty when all ran. |
| `baseline.file_issues` | `report_id` | `{issues: [{number, url, title, created}]}`. Files the failures of a baseline report as issues labeled `forecrew:baseline` (created if missing): one per suite and environment that failed or errored (the failing tests, at most 20, the cells, the commit and the report), and one for all flaky tests. Test and environment names show as inline code. A hidden marker in the body makes it idempotent: an open issue with the marker is found and left alone, so a repeated call, and a later baseline that fails the same way, file nothing twice; a closed one is filed again. |
| `knowledge.seed` | none | `{indexed, sections, curate, text, error}`. Indexes the project's repo knowledge (the mapped paths of its policy, or the default mapping) and outputs the seed sections (`architecture`, `conventions`, `testing`, `runbooks`, `glossary`) that no location covers. `curate` has one item when there are such sections and none otherwise, so a `foreach` over it skips the curation; `text` tells the curator what to fill and what already has a home. A repo that cannot be read, or an installation without a knowledge base, gives `error` and an empty `curate`: onboarding does not depend on it. |
| `knowledge.propose` | `job_id`, `provider` | `{proposals, number, branch, head_sha, reviewers, review_text}`. Sends the job's queued `standalone` proposals to the provider: for the repo, commits them through the GitHub API on `agent/kb-<job>` and opens one PR. Checks each against the policy again: what its proposer may no longer propose is dropped (rejected, audited). `reviewers` are the owner roles that review; `review_text` is the job's `feedback`. A provider without a review flow sends nothing. Idempotent: what was sent stays sent. |
| `knowledge.assess` | `job_id`, `provider`, `reviews` (the foreach of review jobs) | `{if_gate}`. Opens the `knowledge_proposal` gate, with the call's key as ID, for a protected or human-owned section, a provider without a review flow, or a verdict other than `approve`; the proposals get its `gate_id` in the gate's transaction. `if_gate` holds the gate, or nothing when no human decides. |
| `knowledge.decide` | `job_id`, `provider` | `{if_merge, if_follow}`. Anything but an approved gate rejects the proposals and closes the PR. `if_merge` holds the PR when forecrewd merges it, `if_follow` when it is owned by `codeowners`, whose reviewers and a human merge it. Both are empty without an open PR. |
| `knowledge.settle` | `job_id`, `provider` | `{failed}`. Last step and `finally` step: a merged PR or an approved gate accepts the proposals, a closed PR rejects them. In a run that failed or was canceled what is still in review is rejected and the PR closed. |
| `knowledge.access_pr` | `gate_id`, `section`, `level`, `role` | `{opened, number, url}`. When the `access_request` gate was approved and the policy does not grant it yet, opens a PR `agent/kb-access-GATE-n` that adds the role to the section's access list. A human merges it; the policy is never changed directly. |
| `curate.prepare` | none | `{text, if_curate, reverify, stale, notes}`. The curator's work list: entries marked `needs_reverify`, entries not verified for `knowledge.verify_window` and old project notes that no search returned, with excerpts marked as data and each list bounded. `if_curate` has one item when there is something to curate (two curated entries or more, or anything listed) and none otherwise. Reads only. |
| `curate.finish` | `jobs` (the foreach of curator jobs) | `{cleared, dropped, skipped}`. Applies what the curator decided without a proposal, after checking the index: an entry it found still true loses `needs_reverify` and gets `last_verified` now; a project note it named is deleted only when it is old, unread and not promoted. |
| `curate.tools` | `repo`, `integrations` (the output of `discover.github`) | `{opened, skipped}`. For each discovered tool that is new to the profile, or at least three times (and ten) more used than the profile recorded, opens a PR on `.forecrew/profile.yaml`: the tool added as `proposed` with its evidence, or its evidence updated. Once per tool, ever (the audit log remembers it); at most three PRs per run; a profile that would be invalid is not proposed. |
| `goal.retro_context` | `goal_id` | `{text, notes}`. The goal's history for the retro job: spec revisions, what humans answered at the goal's gates, the reviews that asked for changes, the jobs that failed or were canceled, costs by role and the goal's notes with their ids. Everything a person or agent wrote is marked as data, each text cut and each list bounded. Reads only. |
| `goal.learn` | `goal_id` | `{proposals, files, number, branch, head_sha, gate, if_gate}`. The learn step: writes in one PR `agent/learn-GOAL-n` the goal's register files (`.forecrew/assumptions/A-n.yaml`, `docs/decisions/DEC-n.md`, as `registers.Export` gives them, only when they differ from the default branch) and the goal's queued `per_goal` proposals for the repo (`entry`: provenance frontmatter, then the body; source `GOAL-n`). It checks them against the policy again first: what the policy no longer allows, or a section that became protected, is rejected and audited. Proposals for a provider without a review flow wait at the gate. Opens one `knowledge_proposal` gate, with the call's key as ID, for the PR and those proposals; `gate` is its ID (empty when there was nothing to send) and `if_gate` holds it for a `foreach`. Idempotent for its key. |
| `goal.learn_decide` | `goal_id`, `gate` | `{if_merge, if_follow}`. Anything but an approved gate closes the learn PR and rejects the proposals; an approval accepts the ones copied across by hand and says how the PR gets merged (`if_follow` when a section it changes is owned by `codeowners`). |
| `advise.prepare` | none | `{text, signals, room, if_advise}`. Runs the detectors and tends the project's proposals: a snooze that ended puts its proposal back, a proposal done `advisor.MeasureAfter` (14 days) ago gets its `outcome`, an open one its new evidence. `signals` are those nothing answers yet, heaviest first, at most 40; `room` what the weekly cap leaves; `if_advise` has one item when both are there and the role `advisor` has a runtime. Signal text is marked as data in `text`. Idempotent. |
| `advise.file` | `jobs` (the foreach of advisor jobs), `signals` | `{created, updated, dropped}`. Files what the advisor wrote, best first. A draft answers one listed signal, whose key is its `dedupe_key`; evidence and weight come from the cited signals. A draft whose key has an open proposal updates it. One matching a rejected proposal is dropped unless the signal's weight doubled or 90 days passed, one matching a proposal done within 14 days is dropped, and one past `advisor.max_per_week` is dropped. Tools must be in the catalog, which a revision is held to as well: a name in `tools` outside it drops the draft, a draft answering a tooling signal must name a catalog tool, and the action text may neither name a missing tool of the brief that the catalog lacks nor install or run a package or image the catalog lacks (`pip`, `npm`, `pnpm`, `yarn`, `brew`, `apt`, `go`, `cargo` and the like with `install` or `add`, `npx` and `uvx`, `docker run` and `docker pull`; `installRules` in `internal/advisor`). A `config_pr` may only write `.forecrew/profile.yaml`, `matrix.yaml`, `knowledge-policy.yaml` and `environments/<name>.yaml`, valid against their contracts, at most 10 files of 64 KiB. A new proposal gets an `improvement_proposal` gate and a run of `proposal`. Idempotent for its key. |
| `advise.start` | `goal_id`, `gate` (the learn gate, empty when there is none) | `{started}`. Last step of `learn`: starts `advise` unless a run of it is going on for the project, or the learn gate was not approved. An error is logged, never fails the retro. Does nothing without an advisor. |
| `proposal.round` | `proposal_id` | `{gate_id}`: the gate the proposal waits on now. |
| `proposal.assess` | `proposal_id` | `{settled, if_revise}`. `settled` unless the human asked for changes; then `if_revise` holds the advisor's brief, with the proposal and the feedback marked as data. |
| `proposal.reopen` | `proposal_id`, `jobs`, `settled` | `{reopened}`. Nothing when `settled`. Else applies the revision, under the rules of `advise.file`, and opens the next gate; a revision that breaks one is left out and the gate says why. Idempotent for its key. |
| `proposal.carry` | `proposal_id` | `{if_pr, goal_ref}`. For an accepted proposal: a `goal` action creates a goal of kind `feature`, linked through `proposals.goal_id`, and starts it; a `config_pr` action writes the files on `agent/prop-<n>` and opens the PR, which a human merges; a `setup` action waits for `decide_proposal` with `done`. Idempotent. |
| `proposal.pr_ended` | `proposal_id`, `signal` | `{signal}`. `pr.merged` makes the proposal `done`, `pr.closed` `superseded`. |
| `proposal.abandon` | `proposal_id` | `{abandoned}`. The `finally` step: a proposal that is not final is `superseded` and its pending gate closed. |
| `goal.learn_settle` | `goal_id`, optional `skipped` | `{failed, settled}` (`settled` is always true: it ends the learn loop of `goal-v3`). Last step and `finally` step of `learn`: a merged PR accepts what is in review, a closed one rejects it; in a run that failed or was canceled, or with `skipped` (the learn step of `goal-v3` that a human skipped), the PR is closed and what is in review or still queued rejected, a queued proposal even when the PR was merged, since it was never in it. |

### Command jobs

A `job` step with `kind: command` needs `command` instead of `role` and runs
a command without an agent. Its `with` keys are `kind`, `command` (`argv` or
`shell`, `env`, `workdir`, `reports`, `artifacts`), `environment`, `cell`,
`suite`, `overlay` (`ref`, `paths`), `base_ref`, `branch`, `needs`, `timeout`
(default 30m) and the goal, spec and task refs. `role`, `pool`, `prompt` and
`feedback` are refused: the pool follows from the environment. Its output is
empty; the results are the job's test runs (`list_test_runs`).
| `job.if_pushed` | `head_sha` | `{if_pushed}`: the commit when the job step left one, `[]` when it changed nothing. A `foreach` over it skips the steps that need the job's branch, such as a pull request. |

`from`, one `${...}` reference to an object, fills `cell`, `suite`,
`environment`, `needs` and `command` from that object, and `command` is then
not required in `with`. Other keys of the object are ignored and the `with`'s
own keys win. `kind: command` must be literal.

### Running a matrix

`matrix.plan` turns the test matrix into a list a parallel foreach runs, with
no step type of its own:

```yaml
- id: plan
  type: action
  action: matrix.plan
  with: {repo: '${inputs.repo}', ref: '${inputs.sha}', scope: required, suites: '${steps.tests.output.suites}'}
- id: cells
  type: foreach
  parallel: 8
  items: '${steps.plan.output.jobs}'
  steps:
    - {id: run, type: job, with: {kind: command, repo: '${inputs.repo}', base_ref: '${inputs.sha}', from: '${item}'}}
```

`scope` selects `all` (every suite in every cell it applies to), `required`
(required suites in the cells of the matrix's `required`) or `quirks` (suites
with `quirks: true`). `suites` adds those suites in every cell they apply to;
without `scope` only they run. A cell with no suite in scope is left out.

- `cells` has one `{key, cell, environment, pool, needs, suites}` per cell:
  `key` is the axis values as text (`database=pg18,os=linux`), `environment`
  the whole environment, `suites` the matrix's suites. Cells follow the axes
  sorted by name, the last one changing fastest and values in file order, then
  the `include` cells; `exclude` is applied before `include`.
- `jobs` has one `{key, cell, suite, environment, needs, command}` per suite of
  each cell, in the same order, since a command job runs one suite. `key` is
  `<cell key>/<suite>` and `command` carries the suite's `reports` and
  `artifacts`.
- `pool` is `actions` for environments with provider `actions`, otherwise
  `build`. `needs` are the `runs_on` label for `actions`, otherwise the
  environment's `needs` and the cell's `os` axis value, unless it has spaces.
  The job creator adds `docker` for the environments that need it.
- A cell that no environment rule matches, or an environment file that is
  missing or invalid, is an error of the plan.
- A plan has limits, because the matrix file comes from the repo: the axes
  multiplied together, before `exclude`, plus the `include` entries may give
  at most 256 cells, and one plan at most 256 jobs (`matrix.MaxCells`,
  `matrix.MaxJobs`). Above them the plan has `errors` and no cells.

### Locked tests

The tests a test engineer wrote from the spec are locked while the developer
works: `tests.locked_paths` gives the globs, and the developer's `job` step
passes them as `with.locked_paths`. The worker fails the job, pushing nothing,
when a changed file matches (the error names the files), and the prompt lists
them. A developer that believes a locked test is wrong escalates with the
test and the reason (its `escalation`) instead of changing it. That
escalation opens a `needs_you` gate; when a human approves it, the developer
runs again with `locked_paths` empty for that round (`tests.locked_paths` with
`iteration` above 1 and `decision` `approved` gives `[]`), and the lock is back
in the next rounds.
Without approval the lock stays. `goal-v2` wires this
([The goal playbook with testing](#the-goal-playbook-with-testing)).

Limit: the lock covers the files the test engineer wrote. A locked test can
still be neutralised without touching it, for example by a new `conftest.py`
or a change of the runner's configuration. The goal playbook must therefore
treat a new test that is missing from the results at `head` as a failure:
`goal-v2` does, since a criterion whose test did not run in a finished required
cell blocks the verification report ([Verification](#verification)), and the
reviewer sees the diff and the report.

The fail-on-base check shows every new test fails without the change: the new
test files from `head` are run on `base` (`tests.base_check_request`) and
`tests.base_check_result` reads the stored runs.

### Verification

`verification.build` turns the test runs of a PR head into one report
([verification.schema.json](contracts/verification.schema.json)), kept in
`verification_reports` (one per change and `sha`; a rebuild updates it). Rules:

- The latest attempt of a suite in a cell counts (reruns are new jobs of the
  same cell). A test that failed in an earlier attempt and passed in the latest
  is **flaky**: listed with its environments, not blocking. One that failed
  earlier and is skipped or missing in the latest attempt of a required cell
  blocks (the synthetic `(command)`, `(report)` and `(oversized)` results are
  not tests). Runs of the base check (jobs with an overlay) are not read, since
  with stacked PRs the base is another PR's head.
- A required result (a suite in a cell the matrix's `required` and the suite's
  `required` ask for) that failed or errored blocks. Results that are not
  required are shown and never block.
- A required result with no run yet makes the report `incomplete` (and is in
  `blocking`). Anything that blocks otherwise makes it `failed`.
- A criterion with no test in the test map blocks, unless the test engineer
  listed it `untestable` with a reason. A criterion blocks when its tests fail,
  did not run in a run that finished, or were all skipped. The test's result is
  its worst over the required results of its suite (all of them when the suite
  has no required result).
- A test of the test map that is mapped to a criterion the task does not have
  is a test of the change like any other: it blocks when it failed or did not
  run in a finished required cell, and leaves the report `incomplete` while a
  required result is missing. Only a criterion of the task is shown in the
  table.
- A new test that passed on the base branch (`base_check`) blocks; one with
  no `base_check` entry leaves the report `incomplete`. A change whose task
  lists no acceptance criteria blocks, and a sha with no stored run at all is
  `incomplete`.

The report is rendered as Markdown: results (environments by suites with
counts), the traceability table (criterion, tests, result), new tests, flaky
tests and tool versions. The rendering is stored as an artifact (no job; name
`verification/<sha>.md`), posted as one comment per PR, written by the App and starting with a hidden
marker (only the App's own comments are found, since a write token may edit
anyone's) that later builds update in place, only for the PR's current head
(a report of an older sha is stored and gets its check run, but leaves the
comment alone), and shown as the check run `forecrew/verification` on `sha`: `success`
for `passed`, `failure` for `failed`, `neutral` for `incomplete`. Names from the
repo are shown as inline code or escaped text, and tables are cut after 100
rows. The check run needs the GitHub App's **Checks: read & write** permission;
without it forecrewd logs a warning, once, and keeps the comment. The check run is set before the comment.

## Roles

`job` steps name a role from `packs/roles` (format in
`docs/contracts/roles.md`); forecrew.yaml `roles` gives each the runtime and
tier its jobs run with. The prompt of a job is the role's instructions
followed by its context pack (`internal/contextpack`): the job, the project
profile, the goal, spec and task, the step's `prompt` and feedback.

| Role | Writes | Output schema | Context beyond the job |
|------|--------|---------------|------------------------|
| `onboarding-analyst` | `.forecrew/**` | `profile-draft.json` | repo facts |
| `triage` | nothing | `triage.json`: impact, reversibility, areas, reason, assumptions | profile, goal |
| `product-owner` | nothing | `spec.json` | profile, goal |
| `architect` | nothing | `task-breakdown.json` | profile, goal, spec |
| `developer` | everything but `.github/workflows/**` and `.forecrew/**` | `developer-result.json` | profile, goal, spec, task, feedback |
| `reviewer` | nothing | `review-verdict.json` | profile, goal, spec, task |
| `test-architect` | `.forecrew/matrix.yaml`, `.forecrew/test-strategy.md` | `test-plan.json`: summary, every axis with its reason, matrix changes with reasons, assumptions, open questions | profile, spec when given, test plan |
| `environment-engineer` | `.forecrew/environments/**` | `environments.json`: summary, every environment with its provider and reason, assumptions | profile, test plan |
| `knowledge-curator` | `knowledge/**` (never the protected sections or the policy file) | `knowledge-seed.json`: summary, the entries it wrote with their sources, the sections it skipped, assumptions | profile. Two read-only tasks, `retro` (`knowledge-curator.retro.json`: summary, the proposals it made, assumptions) and `curate` (`knowledge-curator.curate.json`: summary, `reverified` entries, `drop_notes`, proposals, assumptions); both read the history or the work list in their `feedback` and propose with `kb_propose` |
| `advisor` | nothing | `proposals.json`: summary, ranked proposals, assumptions | the signals, proposals and catalog in its `feedback`. Read-only task `revise` (`advisor.revise.json`: summary, the proposal written again, assumptions). Pool `control`, best tier |
| `test-engineer` | test files only: `**/*_test.*`, `**/*.test.*`, `**/test_*.*`, `*.spec.*` and `*_spec.*` with code extensions, and `tests/`, `test/`, `__tests__/` and `testdata/` directories; never `.forecrew/**`, `.github/**`, `vendor/`, `node_modules/`, nor dependency, lock, build, Docker, script or CI files at any depth (`go.mod`, `package.json`, `*.lock`, `Makefile`, `Dockerfile`, `*.sh`, ...) | `test-map.json`: per test its criterion, suite, JUnit id (`<classname>::<name>`) and file; every file created or changed (`files`, the paths later locked for the developer and applied on the base); untestable criteria with reasons; assumptions | profile, goal, spec, task with its criteria, test plan |

Every output also has `escalation` (null unless the role stopped for a
reason its `escalate_when` lists). The test plan section holds
`.forecrew/matrix.yaml`, `.forecrew/test-strategy.md` and every environment
file the matrix's rules name, read from GitHub at the job branch when the
matrix is there and at the base otherwise; files not written yet are marked
missing. The `onboarding-tests` playbook runs `test-architect` and `environment-engineer`; the goal playbook with testing (`goal-v2`) runs `test-engineer`, and `test-architect` when the matrix must change for a task.

The council roles (`facilitator`, `devils-advocate`, `market-researcher`,
`security-reviewer`, `ux`) and the council packs that seat them, with other
roles in a council mode, are described in [councils.md](councils.md). They
run in the `council` playbook.

## The learn playbook

`packs/playbooks/learn.yaml`, inputs `repo` and `goal_id`, closes a goal that is
done with a retro. `goal-v3` runs its steps itself, in a loop with
`on_failure: skip`, before closing the goal (a file cannot include another, so
`TestGoalV3LearnIsTheLearnPlaybooks` keeps the copy equal); started alone, as
councils are, it is the same retro for a goal that is done.

1. `goal.retro_context` writes the goal's history.
2. A `knowledge-curator` job with `task: retro` reads it and the code, searches
   the knowledge base and proposes lessons and other entries with `kb_propose`,
   at most eight, citing in a reason the notes worth keeping as `note:<id>`
   (they are promoted). Its proposals belong to the goal (`per_goal`), as do the
   ones the goal's other jobs made, and wait.
3. `goal.learn` writes the PR `agent/learn-GOAL-n` and opens the
   `knowledge_proposal` gate. The gate is always a human's: the PR also holds the
   registers, which have no owner role.
4. After the decision `goal.learn_decide`, the merge after the checks of the
   head that was approved (`github.merge_pr`), or CODEOWNERS and a human on
   GitHub, and `goal.learn_settle`.

## The curate playbook

`packs/playbooks/curate.yaml`, input `repo`, is started by a schedule, weekly
for every onboarded project unless `schedules` in config.md says otherwise: `curate.prepare`, a `knowledge-curator` job with
`task: curate` when there is something to curate, `curate.finish`,
`discover.github` and `curate.tools`. See [knowledge.md](knowledge.md#upkeep).

## The advise playbook

`packs/playbooks/advise.yaml`, input `repo`, is started weekly by the default
schedule and by `advise.start` at the end of `learn`: `advise.prepare`, an
`advisor` job when there is something to answer and room for it, and
`advise.file`. Each new proposal then has a run of
`packs/playbooks/proposal.yaml` (inputs `repo` and `proposal_id`, the run's ID
being the proposal's) that waits on its gate, has the advisor revise it after a
request for changes (in four rounds at most; a proposal still unsettled then is
superseded), carries out the action and, for a
PR, waits for it. `goal.close` makes the proposal of a goal `done` and
`goal.settle` `superseded`. A proposal with a `measure` is measured by the
first `advise.prepare` 14 days after it is done: the weight of its signal then,
zero when the signal is gone, is `outcome.after`. `goal.retro_context` shows
the outcomes of the last 30 days to the next retros.

Gates and proposals move together: `advisor.Service` is a `gates.Recorder`, so
`approve` or `reject` on the gate and `decide_proposal` do the same thing, and
a human's decision is recorded as a decision (`DEC-n`). A snooze leaves the
gate pending and out of `list_pending_gates` until the date.

The detectors (`internal/advisor`) are deterministic. The key of a signal is
its proposal's `dedupe_key`, and its weight the proposal's measure. A subject
from the repo or an agent longer than 64 bytes keeps its first 40 and a hash of
the whole, and a knowledge cluster whose questions share no word is no signal:

| Detector | Signal | Key | Weight |
|----------|--------|-----|--------|
| `workers` | jobs of the last 14 days that waited over an hour for a pool or labels no online worker has | `workers:<pool>:<labels>` | jobs |
| `tooling` | a tool named twice or more in the last 90 days in `needs_you` gates, escalations and `ask_human` questions alike | `tooling:<tool>` | mentions |
| `knowledge` | `ask_human` questions of two or more goals in the last 90 days with half their words in common | `knowledge:<shared words>` | goals |
| `testing` | two or more criteria `untested` or `untestable`; a test flaky in three or more reports; a required suite with no result in three or more reports and most of them | `testing:untested`, `testing:flaky:<test>`, `testing:incomplete:<environment>/<suite>` | occurrences |
| `rework` | review comments of one theme, nits left out, on three or more tasks in the last 90 days | `rework:<theme>` | tasks |
| `cost` | a role with 20 or more jobs on merged tasks that costs over twice the median of the roles per merged PR | `cost:<role>` | its ratio to the median |
| `observability` | a Go or Node component with no known metrics, tracing or error-tracking library, in a project with no monitoring tool approved or connected | `observability:<path>` | 1 |
| `security` | nothing in CI or the update-bot configs scans dependencies, or secrets | `security:dependency-scanning`, `security:secret-scanning` | 1 |
| `ci` | a platform of the matrix (else of the profile) no CI runner matches | `ci:platform:<platform>` | 1 |
| `integrations` | a tool `approved` and not connected 30 days after the last approved profile | `integrations:<tool>` | periods of 30 days |

A missing tool is recognized in text by these rules, in `toolRules`: `X: command
not found`, `command not found: X`, `exec: "X": executable file not found`, `X is
not installed`, `missing tool|command|binary|service|environment X`, `no|without
access to X`, `cannot|could not|can't connect|reach (to) X`, and
`needs|requires (a|an|the) X tool|cli|service|server|environment`. A review
comment's theme comes from keywords, in `themes`: `security`, `error-handling`,
`concurrency`, `tests`, `naming`, `duplication`, `docs` and `logging`. The
reviewer's output has a severity per comment and no category.

## The onboarding playbook

`packs/playbooks/onboarding.yaml` takes `repo` and `project`. Right after
registering, `discover.github` reads the tools the team uses from GitHub; its
text goes to the analyst as feedback, next to the repo facts' integrations
(found by `analyzer.Local` from the MCP configs, the links in the docs and
templates, Backstage and TechDocs files, CI notify and release steps and issue
keys in branches and recent commits). The analyst keeps, corrects or drops
them in the profile's `integrations`, all with status `proposed`, and the
human approves them in the profile PR. An `onboarding-analyst` job writes `.forecrew/profile.yaml` on
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
its `finally` step puts the project back to registered (onboarded, when the
profile was stored already).

After `onboarding.complete` the playbook seeds the knowledge base
([knowledge.md](knowledge.md)): `knowledge.seed`, then, only when some seed
section has no location, a `knowledge-curator` job on `agent/kb-seed` that
proposes entries for those sections from `CLAUDE.md`, `AGENTS.md`, the README
and `docs/`, a PR, and a `knowledge_proposal` gate on that PR. Approving it
merges the PR; merging it on GitHub approves it and closing it rejects it
(which ends the run failed; the project stays onboarded). A curator that
writes nothing leaves no commit: `job.if_pushed` then skips the PR and the gate
and the run succeeds. The curator role needs
an entry under `roles` in forecrew.yaml; without one the job fails and a
`needs_you` gate opens.

### Tests: strategy, matrix, environments and baseline

`onboarding.complete` stores the profile but keeps the project `onboarding`
(`hold`). The next step, `onboarding.start_tests`, starts the
`onboarding-tests` playbook (`packs/playbooks/onboarding-tests.yaml`, inputs
`repo` and `project`) as a run of its own, so the same playbook serves a
refresh. Without entries for `test-architect` and `environment-engineer`
under `roles` in forecrew.yaml there is no tests part: the project is onboarded
right there, as before. The profile part goes on to seed the knowledge base
meanwhile.

1. **Plan.** A `test-architect` job and then an `environment-engineer` job
   write `.forecrew/matrix.yaml`, `.forecrew/test-strategy.md` and the
   environment files on a branch of the run's own (`onboarding.tests_branch`).
   `matrix.plan` (scope `all`) validates the matrix and the environment files
   it names. Nothing to commit (the repo has its test plan) means no PR and no
   smoke: the baseline runs on the default branch.
2. **Smoke.** When the jobs changed something, `actions.install_workflow` adds
   `.github/workflows/forecrew-matrix.yml` to the branch if any environment of
   the matrix uses `actions`, and `tests.smoke_plan` gives one command job per
   added or changed environment. Its command is the environment's `versions`
   commands, so `tests.smoke_result` reads their exit codes. The errors of
   `matrix.plan`, a failing versions command and an escalation of either role go
   back to both roles once as feedback (like `profile.validate`); when the two
   tries run out a `needs_you` gate opens, and approving it gives one more
   round. Not covered: a job that fails because its environment cannot be built
   or set up (a bad Dockerfile, a failing setup command) is a failed job, not a
   result: it opens a `needs_you` gate that says why, and approving it runs the
   job again (the engine cannot loop a failed job back as feedback). A dispatch
   needs the workflow on the default branch, so environments on `actions` are
   not smoked until it is there; the baseline exercises them after the merge.
3. **Approval.** One PR holds both outputs. After its checks, a
   `test_plan_approval` gate opens on it: approving merges it, only at the head that
   was proposed (a commit pushed while the gate was pending is not merged: the
   step escalates; the PR can add a workflow), and merging it on GitHub approves
   and closing it rejects, as for the profile. Changes requested
   start another round with the feedback (at most three before a `needs_you`
   gate).
4. **Baseline.** `matrix.plan` with scope `all` on the merge commit, a
   parallel foreach of command jobs (at most 8 at once), then
   `verification.build` with `baseline: true`: no criteria and no base check,
   no PR comment and no check run. A required cell that failed runs once more
   (`verification.rerun_cells`), so a test that passes the second time is
   flaky, and the report is built again. The report is stored on the project
   by commit (a report belongs to a change, so under the tests PR or, with no
   PR, the project's latest merged one).
5. **Known failures.** `baseline.file_issues` files one issue per failing suite
   and environment and one for the flaky tests, labeled `forecrew:baseline`,
   each once. Failures do not block anything.
6. **Done.** `onboarding.finish` moves the project to onboarded, after the
   baseline ran.

The steps of this part that can fail for a reason a human must read (the
registration, the jobs, the actions that read GitHub or the repo, the PR, the
merge, the baseline and its report) escalate, so their failure opens a
`needs_you` gate that says why; rejecting it ends the run, and its `finally`
step puts the project to onboarded, not back to registered: its profile is
stored, so it is onboarded without a baseline. A PR closed on GitHub, or a
`test_plan_approval` gate rejected, also ends the run (a closed PR is a
rejection) and has no gate of its own that explains it; the project is
onboarded without a baseline then too. The same rule holds for the onboarding
run: a failure after `onboarding.complete` (the knowledge seed) leaves the
project onboarded.

`onboard_project` with `refresh: true` ([mcp.md](mcp.md)) starts
`onboarding-tests` on a project that is onboarded, to propose a new test plan
if one is needed and run a new baseline; the project stays onboarded
throughout. A refresh already running for the project is refused, and so is
one when forecrew.yaml has no entry for `test-architect` or
`environment-engineer` under `roles`. Each refresh uses a branch of its own.

## Councils

`packs/playbooks/council.yaml` takes `council_id` and `roles` (the members
instead of the kind's, or `[]`) and runs the protocol of
[councils.md](councils.md): framing, independent proposals, critiques, up to
`max_rounds` of a check and revisions while positions move, the synthesis and
the decision. `start_council` starts it for a council of its own.

A goal playbook runs a council with an action and a wait:

```yaml
- {id: ask, type: action, action: council.run, with: {kind: technical, question: How should we build it?}}
- {id: answer, type: wait, with: {signal: council.decided, council_id: '${steps.ask.output.council_id}'}}
```

There is no `council` step type: a council needs nothing the engine lacks,
because a signal wakes any run waiting for its key, whichever run published it.
A step type would add to the engine's replay what an action and a wait already
do, and the crash between the two is covered by the IDs (`council.run`). The
wait fails when the council fails or is canceled, so `on_failure` applies. When
the goal's run ends, `goal.settle` cancels its running councils: the actions of
the council's run then fail, so the jobs of the stage under way finish and no
further job starts. They are not canceled, because a failed job escalates to a
human. A council waiting at its gate stays there until the gate is resolved,
spending nothing.

- **Jobs.** Every job has the `council` key (`workflow.CouncilTurn`), which makes
  it a step's job (`councils.Set.Job`) on the member's pinned runtime at the
  council's commit. An invalid output reruns the job once with what is wrong as
  feedback; after that a needs_you gate asks, and approving it gives one more try.
  A job that fails also escalates. Rejecting either cancels the council.
- **Step ids.** The job step ids and the loops `debate` and `decision` name the
  stage and round `council_read` tells from a job's step run, since a job has no
  field for its council. `internal/jobmcp` tests that the playbook keeps them.
- **Budget.** The tokens (input and output) of every job of the run and the wall
  time since the council started, checked between stages, so a stage that started
  finishes. Once it ran out the rest of the debate is skipped and the facilitator
  synthesizes. The wait for a human does not count as time, and the human's round
  runs whatever the budget says.
- **Decision.** `policy.accept_recommendations` (config.md) names the kinds whose
  briefs are accepted without a human, recorded as `policy`, when the facilitator
  calls the decision reversible and only the devil's advocate dissents; by default
  every brief opens a `council_decision` gate. `council.decide` trusts none of its
  inputs: it asks the policy again, or reads the gate, which must be the council's
  and have the decision it was given. The gate is opened by an action and
  followed by a wait for `gate.resolved`, because a rejection fails the council
  instead of canceling the run and the council records its gate while it is open.
  A brief written after a human's round always goes back to a gate, and that gate
  refuses `request_changes`.
- **Decisions.** Every decision is in the register once. A human's is stored by
  the gate's resolution, in its transaction, with the council's ID: an approval
  as the recommendation, `changes_requested` and `rejected` under those names, so
  a council can have one per gate. The policy's is stored by
  `AcceptRecommendation`, which finds it when called again. `council.decide` looks
  the decision up first and moves the council afterwards, so after a crash a
  repeated call records nothing, and a decided council always has its decision.

## The goal playbook

`packs/playbooks/goal.yaml` takes `repo`, `goal_id` and `platforms` (the
goal's worker labels); `create_goal` starts it.

Sizing: the `triage` role and `goal.size` decide which councils a goal gets
(config.md, sizing). A goal created with `councils` (`product`, `technical`,
or `[]` for none) keeps that size and skips the triage job. This playbook does
not run triage; [`goal-v3`](#the-goal-playbook-with-councils-and-learning) does.

1. **Intake.** `goal.intake` opens the goal's GitHub issue.
2. **Spec.** A loop of at most three rounds (a needs_you gate for the next
   ones): a read-only `product-owner` job, `goal.spec` (a new spec revision,
   rendered into the issue: criteria `AC-n`, scope, assumptions with their
   impact, open questions) and a `spec_approval` gate. Changes requested
   start a new round with the feedback.
3. **Plan.** An `architect` job and `goal.plan`: one task and one sub-issue
   per task, in order. A plan that cannot be used, such as one that leaves
   acceptance criteria uncovered, is sent back to the architect with what is
   wrong; after a second bad plan a needs_you gate opens, and approving it
   reruns the architect.
4. **Tasks, one after the other.** A delivery loop holds a build loop and the
   merge gate.
   - Build, at most three rounds (a needs_you gate for the next ones):
     `goal.round`, a `developer` job on branch `agent/TASK-n` on a worker with
     the task's `needs` and the goal's platforms, `goal.pr_text`, the PR
     (`github.open_or_update_pr`), then, while the PR is open, a wait for
     `checks.completed` on the commit the developer pushed (`none` means the
     repo has no checks),
     a read-only `reviewer` job on the same branch (a different vendor
     according to the config) and `goal.review_post`. `goal.assess` ends the
     round when the PR is ready or merged; failing checks or a request for
     changes start the next round with their feedback as the developer's.
   - Assumptions: the assumptions in a job's output are registered when the
     job succeeds (docs/mcp.md, registers), each high-impact one with an
     `assumption_review` gate. After the review, a loop of `goal.hold` and a
     `wait` per gate holds the round until a human has accepted or rejected
     the high-impact assumptions the task rests on (its own and the goal's);
     `goal.assess` then keeps the merge gate closed for any still unverified,
     and sends the task back to the developer, with the evidence, for one a
     human invalidated since the developer job started. A wait ends when its
     gate is resolved; a PR merged or closed on GitHub sends back the reviews
     of its task, which ends the wait, and a wait nobody answers opens a
     `needs_you` gate after 72 hours.
   - Merge gate: a `merge_approval` gate with the checks, the verdict, the
     criteria check and the assumptions. Approving it squash-merges
     (`github.merge_pr`, which needs checks that are not failing or running);
     merging on GitHub approves, closing the PR rejects. Changes requested
     start a new delivery, at most five (then a needs_you gate), with the
     feedback.
   - The reviewer only comments; nothing is merged without an approved gate
     or a human merge. A PR merged on GitHub meanwhile ends the task and skips
     the steps that follow. After an approval a failing merge opens a
     needs_you gate.
5. **Close.** `goal.close` comments the summary and the cost of all jobs on
   the goal issue and closes it.

A run that fails or is canceled runs `goal.settle`: the goal is canceled and
its unmerged tasks and the issues are closed; a task whose PR is merged stays
merged.

### Limits

- A developer that reports an escalation leaves no PR. `goal.pr_text`
  comments the reason on the task's issue and a needs_you gate asks a human;
  approving it reruns the developer. The gate text is the generic one of a
  loop that ran out of rounds, the reason is on the task issue, and what the
  human answers cannot reach the rerun (an escalation's feedback does not pass
  to a loop's extra round).
- The merge gate is approved against the PR as it stands. Commits that humans
  push after the review are covered by the human's approval and by the
  checks (`github.merge_pr` refuses failing or running ones), not by the
  reviewer.
- An issue is created, then its number is recorded. If forecrewd dies between
  the two, the repeated action opens a second issue for the goal or the task;
  the tracker offers no search to find the first.

## The goal playbook with testing

`packs/playbooks/goal-v2.yaml` is the goal playbook with the testing system. It
takes the same inputs as `goal`. It is off for every project: `create_goal`
starts it only for the projects listed in `testing.projects` of forecrew.yaml
([config.md](contracts/config.md)). Every other project, and every goal that
already runs, keeps `goal`, whose file did not change.

It is `goal` with these differences, task by task:

1. **Tests before the code.** A loop of at most two rounds (a needs_you gate
   for a third): a `test-engineer` job on the task's branch `agent/TASK-n`,
   which writes tests only, and `tests.map`, which says whether the tests are
   ready (the engineer did not escalate and every criterion has a test or an
   untestable reason). An engineer that stops because the matrix or a suite
   must change first (its escalation mentions the matrix or a suite) is
   answered by a `test-architect` job on the same branch, and the next round of
   the engineer gets its reason as feedback. The architect's task has no field
   that says the matrix must change; only the engineer's report does. Rule of
   thumb: the matrix changes only when a task adds a platform, service or
   suite.
2. **Locked developer rounds.** `tests.locked_paths` gives the developer's job
   the files of every test engineer job of the task as `locked_paths`. A
   developer that changes one fails its job, which opens a needs_you gate whose
   text names the files; approving it reruns the job, with the lock. A
   developer that believes a locked test is wrong escalates instead: a
   needs_you gate shows the escalation (quoted as untrusted data) and says that
   approving drops the lock on the tests for one round. Approving reruns the
   developer without the lock, with the human's note as feedback; the lock is
   back in the next round, and the merge gate's summary says that a round ran
   without it. Rejecting stops the run. A needs_you gate has no "request
   changes", so there is no way to rerun with the lock and a note from that
   gate; a human who wants that rejects it or answers on the task's issue.
3. **Verification of every head**, after the checks of the head completed, in a
   loop of at most three rounds:
   - `matrix.plan` (scope `required`, plus the suites of the new tests, with
     `only_runnable`) and a parallel `foreach` of command jobs on the head, at
     most eight at a time;
   - `tests.base_check_request` (with `only_runnable`), a parallel `foreach`
     of its jobs and `tests.base_check_result`, which compares the runs with the
     requests, so a new test whose base check did not run everywhere is not
     checked;
   - `verification.build`, `verification.rerun_cells` and a `foreach` of the
     failed required cells once more, `verification.build` again, and
     `verification.read`.

   A report with the status `incomplete` after that (a cell or a base check no
   online worker can run, which is never started, because a queued job nobody
   can lease would hold the run forever) opens a needs_you gate that names the
   missing pools and labels. Bring a worker online and approve: the matrix runs
   again. A `failed` report is complete: it is rework. A job of the matrix that
   fails (an environment that cannot be built, a timeout) opens a needs_you
   gate, and approving it runs the job again.
4. **Review and assess.** The reviewer's job gets the report's summary and what
   blocks as feedback (what comes from the repo is quoted as untrusted data).
   `goal.assess` runs with `require_verification`: a `failed` report is
   rework feedback for the developer with the blocking reasons, and the
   `merge_approval` gate's summary has the report's status and where to read it.
5. **Merge.** `github.merge_pr` runs with `sha`, the head that was verified and
   approved, and with `require_verification`: it merges only that head, and only
   when its stored report has the status `passed`. The report's status
   decides, not the check run `forecrew/verification`, because an incomplete
   report is a neutral check run and neutral counts as passed in the check
   status.

### Limits of goal-v2

- The report gates the merge this playbook does. A human who merges the PR on
  GitHub is not stopped: `goal.assess` and `github.merge_pr` return early for
  a merged PR, so that merge skips the report. A head that moved after the
  approval is not merged either (`sha`): a human must then reject the gate of
  the failed merge, or merge the PR on GitHub, since the playbook does not
  verify the new head by itself.
- Per task, at most two rounds of test design before the developer, then at
  most three build rounds per delivery and five deliveries, as in `goal`.
- Per round, the verification starts at most one command job per cell and
  suite of the matrix (at most 256, `matrix.MaxJobs`), one per suite and
  environment of the new tests for the base check, and one rerun of each
  failed required result, and the loop may repeat that three times (more after
  a gate approves it). Mind the cost of a large matrix: it multiplies by the
  rounds, and by the minutes of the `actions` pool.
- The lock covers the files of the test engineer, not a new `conftest.py` or a
  change of the runner's configuration, which the reviewer and the base check
  must catch (see [Locked tests](#locked-tests)).
- An environment the matrix needs and the repo lacks stays a failure of
  `matrix.plan` or `verification.build` for a human; the playbook does not run
  the `environment-engineer`.

## The goal playbook with councils and learning

`packs/playbooks/goal-v3.yaml` is the full feature workflow: triage, the product
council, the spec gate, the technical council, the ADR, the tasks, test design,
implementation, verification, review, the merge gate and the learn step. It
takes the inputs of `goal`. It is off for every project: `create_goal` starts it
only for the projects listed in `goal_v3.projects` of forecrew.yaml
([config.md](contracts/config.md)), also when they are in `testing.projects`
(and such a project reports bugs, since it runs the testing system);
every other project, and every goal that already runs, keeps its playbook, and
the files of `goal` and `goal-v2` did not change. It was built as a new version
because it changes the loop a pilot may be running on.

```
intake -> sized? -> triage job -> goal.size
              (skipped when a human chose the councils)
 -> product council -> spec loop (spec_approval gate, brief linked)
 -> technical council -> architect (ADR) -> goal.plan -> ADR PR (merge_approval)
 -> per task: test design -> delivery (as goal-v2: locked tests, matrix,
    verification report, review, assess, merge_approval, merge)
 -> learn (retro job, goal.learn PR, knowledge_proposal gate, merge)
 -> goal.close
```

It is `goal-v2` with these steps added:

1. **Triage.** `goal.untriaged` says whether the goal has no size (a goal
   created with `councils`, even `[]`, has one); a `foreach` over its `if_triage`
   runs the `triage` job, and `goal.size` stores the councils the sizing rule
   (config.md, sizing) gives the triage, or keeps the human's. A small, reversible
   goal gets no council and goes on to the spec.
2. **Product council**, a `foreach` over `if_product`: `goal.council_constraints`,
   `council.run` with the question "Should we build this, and what exactly?" and
   a wait on `council.decided`. The council's brief is in the `decisions` the
   product owner's job reads (`contextpack`: the briefs of the goal's decided
   councils are listed with its decisions, as untrusted agent text), and the
   spec gate's summary names it and quotes its recommendation as agent text
   (`goal.spec`). A council that fails, for example
   a brief a human rejected, stops the run: do not build what the council
   said not to.
3. **Technical council**, a `foreach` over `if_technical`, after the spec gate
   (it needs the approved spec) with the question "How should we build it?". Its
   members read the goal's latest spec, which `contextpack` puts in their pack,
   and its brief and its decision feed the architect's job in the same way.
4. **ADR.** The decision of a goal is architectural when its technical council
   decided: that council weighs the design. `goal.adr` then gives the
   architect's job `write_paths: [docs/adr/**]`, the `build` pool (the `control`
   pool has no write token) and the branch `agent/adr-GOAL-n`, and the job's
   prompt asks for the record; without that council the job stays read-only.
   A repo whose policy keeps its ADRs in a protected section, or in a section
   wider than `docs/adr/`, gets a read-only architect and no ADR (`goal.adr`).
   After the plan, a loop of at most three rounds (a needs_you gate for the next
   ones) reviews the ADR: `goal.adr_ready` finds the branch (an architect may
   write nothing), the PR is opened, waits for its checks, and a `merge_approval`
   gate merges it (`github.merge_pr` with the head `sha`). Requesting changes
   runs the architect again on the same branch with the feedback and reviews the
   new head; rejecting the gate stops the run and leaves the PR open. This is a docs PR, so the merge does not need a
   verification report, unlike the tasks'.
5. **Tasks.** The delivery is `goal-v2`'s, step for step
   (`TestBugfixDeliveryIsGoalV2s` keeps `goal-v2`, `goal-v3` and `bugfix` equal):
   tests written from the spec and locked for the developer, a verification
   report on every PR head, review, `goal.assess` and `github.merge_pr` with
   `require_verification` and the approved `sha`. `TestGoalV3KeepsTheMergeGate`
   fails when a change drops one of them. The merge gate stays closed while a
   high-impact assumption of the task is unverified (`goal.assess`, tested end to
   end by `TestGoalV3HighImpactAssumptionBlocksTheMergeGate`).
6. **Learn**, after every task is merged and before `goal.close`: a loop with
   `on_failure: skip` that holds the steps of the learn playbook. The PR
   `agent/learn-GOAL-n` is open, with its `knowledge_proposal` gate, before the
   goal closes. A failure in it (the retro job, GitHub, the merge) does not fail
   the goal: a `needs_you` gate shows it, approving skips the learn step
   (`goal.learn_settle` with `skipped` closes its PR and rejects its proposals)
   and the goal closes; rejecting stops the run like any other.
   The last step of the loop, as in `learn`, is `advise.start`: an approved
   learn step, or one with nothing to approve, starts the `advise` playbook; a
   skipped one never reaches it, and a rejected learn gate starts nothing.

A goal whose run ends failed or canceled runs `goal.settle`, which also rejects
the goal's knowledge proposals that waited for a learn step that will not run.

### Limits of goal-v3

- **Cost.** A council is four or five members, a facilitator and up to two
  revision rounds (councils.md); its pack caps the tokens and the time: 600,000
  tokens and 60 minutes for a product council, 800,000 and 90 minutes for a
  technical one. A goal sized in for both runs two, besides triage (a `fast` model, at most 15 turns), the ADR in the architect's
  job and the retro. Raise `sizing` thresholds to size fewer goals in; a human who
  creates a goal with `councils: []` pays for none.
- The constraints of a council are the goal's platforms only; the goal's own
  text reaches the members as data.
- The ADR is written only after a technical council; an architectural decision
  of a goal sized without one is not recorded.
- Rejecting the ADR gate cancels the run, as every gate does. A rework round runs the whole architect job again, and the plan it returns is ignored: the plan stored before stays.
- A learn step skipped by a human leaves the register files and the proposals
  unsent: the goal is done without its retro.
- The limits of [goal-v2](#limits-of-goal-v2) apply to the delivery.

## Bug-fix playbook

`packs/playbooks/bugfix.yaml` fixes a bug: reproduce, a failing regression test,
the fix, verification, review, and a merge with your approval. `report_bug`
(MCP, `POST /api/v1/bugs`, `forecrew bugs report`) creates a goal of kind `bug`
(`goals.kind`) and starts it; so does `@forecrew bug` in a comment on an issue,
which makes that issue the goal's issue. It uses the testing system, so like
`goal-v2` it is only for the projects listed in `testing.projects`; for any
other `report_bug` answers with an error and `@forecrew bug` replies with it.
It takes the inputs of `goal`. `goal` and `goal-v2` did not change.

1. **Intake.** `goal.intake` opens the goal's issue labeled `forecrew:goal` and
   `forecrew:bug`, or labels the issue the command was given on.
2. **Report as spec, bug as task.** `bug.spec` stores the report as the spec
   with the one criterion `AC-1`, and `bug.plan` stores one task and its issue.
   There is no product owner, no architect and no spec approval: the report is
   the spec. The task exists before the test because the test engineer, the
   lock and the verification all work on a task and its branch `agent/TASK-n`.
3. **Reproduce.** A loop of at most three rounds (a needs_you gate for a
   fourth): a `test-engineer` job writes one test that reproduces the bug, or
   escalates with `cannot_reproduce:` and what it tried; `tests.map` checks its
   output; the tests run on the default branch with the test files laid over it
   (`tests.base_check_request`, command jobs, `tests.base_check_result`), for
   tests that are ready only (`tests.map` with `repo` also reads the matrix, so a
   suite it lacks is not ready, and `cannot_reproduce` has no head to check).
   Then `bug.reproduced` decides. Only the latest round's test counts, whatever
   its name, for the base check, the lock and the verification; the files of
   the earlier rounds that are still on the task's branch stay in the overlay
   and the lock (a retry lists only what it changed). When the test fails on base the round is good.
   Otherwise (the test passes on base, so it does not reproduce the bug; no
   test; `cannot_reproduce`; a base check no worker could run) a needs_you gate
   shows the attempt, quoted as untrusted data. The gate is about the goal's issue, so `@forecrew approve` there finds it. Approve it with your note (steps,
   versions, logs) to run the test engineer again with the reason and the note (`bug.brief`);
   reject it to stop the run. A needs_you gate has no "request changes"
   ([permissions.md](contracts/permissions.md)), so the note travels with
   approve.
4. **Fix.** The delivery loop of `goal-v2`, step for step: the developer with
   the regression test locked, the checks, the verification of the head (the
   matrix, the base check again and the report), the reviewer, the human
   assumption reviews and the merge gate. `github.merge_pr` merges only the
   verified head with a passed report.
5. **Close.** `goal.close` comments the summary on the issue, with a
   "Regression tests" list naming the tests, and closes it. A rejected or failed
   run is settled by `goal.settle`, as in `goal`.

The engine cannot include steps from another file, so the delivery loop is
written out again in `bugfix.yaml`; a test fails when it differs from the one
in `goal-v2.yaml`, so change both together. An engine `include` would remove
the copy.
