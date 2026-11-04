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
| `job` | `with`, `on_failure` | Creates a job through `workflow.JobCreator`, then waits for its `job.completed`. `with` takes the json keys of `workflow.JobRequest` (`role` required; `account_id`, `project_id` are set by the engine; `goal_id` and `task_id` default to the run's; `pool` picks the worker pool and defaults to the role pack's `pool`, and a pool whose policy does not run the role fails the step (docs/worker.md, Pools); `prompt` adds instructions to the job's prompt and must be literal text written in the playbook: a `${...}` reference in it is refused when the playbook loads, because the prompt is rendered as trusted instructions; `locked_paths`, a list of globs, goes into the job spec and the prompt: the job fails if it changes a matching file, see Locked tests). Fails when the job fails or is canceled. |
| `action` | `action`, `with`, `on_failure` | Calls the Go function registered under `action` in `workflow.Actions` with the resolved `with`. Its result is the step output; an error fails the step. |
| `gate` | `with`, `on_failure` | Opens a gate and waits for `gate.resolved`. `with` takes the json keys of `gates.OpenRequest`: `kind`, `title`, `subject_repo` (required), `summary`, `subject_number`, `goal_id`, `spec_id`, `task_id`, `job_id`. |
| `wait` | `with`, `timeout`, `on_failure` | Waits for a signal. `with.signal` is the kind and the remaining keys build its key with the `domain` helpers: `checks.completed` (`repo`, `sha`), `pr.merged` / `pr.closed` (`repo`, `number`), `job.completed` (`job_id`), `gate.resolved` (`gate_id`). The first signal stored under the key completes the step, so for a PR either merged or closed does (the output's `signal` says which). On `job.completed` the step follows the job-step rules: it outputs the job's values and fails when the job did not succeed. On `gate.resolved` it outputs the gate's values but does not apply the gate-decision rules below. A wait for `gate.resolved` (like a gate step) finds the gate resolved when the signal was lost; the other waits have no lost-signal fallback. `timeout` (Go duration, e.g. `30m`) fails the step after that long; zero waits forever. |
| `foreach` | `items`, `parallel`, `steps` | Runs `steps` once per element of the list `items` (a single `${...}`), in order, or with `parallel: <n>` at most n items at a time (see [Parallel foreach](#parallel-foreach)). Inside, `${item}` and `${index}` (from 0) are set. |
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
- **Items of a parallel foreach** resume the same way, each from its own step
  runs.
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
| `github.open_or_update_pr` | `repo`, `title`, `body`, `head`, `base`, optional `previous`, `task_id` (defaults to the run's task) | `{number, url, head_sha}`. Updates the open PR from `head` or opens one, upserts its `changes` row and adds it (`owner/name#number`) as a dependent of the assumptions the task rests on. With `previous`, the number of an earlier round's PR, it fails when a human closed that PR instead of opening another, and when a human merged it outputs `merged: true` with `merge_sha` and changes nothing. `if_open` is `[number]`, or `[]` for a merged PR, for a `foreach` that skips steps. |
| `github.merge_pr` | `repo`, `number`, optional `check_profile` | `{merged, already_merged, sha}` (the merge commit). Squash-merges unless already merged. Only a head whose checks succeeded (or has none) is merged; it fails on failed or running checks, and with `check_profile` when the profile file at the head is invalid. Use `on_failure: escalate`. |
| `actions.install_workflow` | `repo`, `branch` | `{path, changed}`. Writes the `forecrew-matrix` workflow (`packs/workflows`) to `.github/workflows/forecrew-matrix.yml` on `branch`, which command jobs of pool `actions` dispatch (actions.md). `changed` is false when the file is already identical. Needs the App's Contents and Workflows write permissions. |
| `goal.intake` | `repo`, `goal_id` | `{issue_number, issue_url, default_branch}`. Creates the labels `forecrew:goal` and `forecrew:task` if missing and opens the goal's issue, labeled `forecrew:goal`, unless the goal has one. |
| `goal.size` | `goal_id`, `triage` | `{councils, if_product, if_technical}`. A goal without a size takes it from `triage`, the `triage` job's output: the councils the sizing rule gives (config.md, sizing) are stored as `goals.size`. A goal that has a size keeps it and `triage` is not read: a human's override (create_goal with `councils`) is such a size, so that goal skips the triage job. `if_product` and `if_technical` are `[goal_id]` when the goal gets that council, else `[]`, for a `foreach` that skips the council steps. Refuses a triage with an unknown grade, no area or no reason. |
| `goal.spec` | `goal_id`, `job_id`, `spec` | `{spec_id, revision, summary}`. Stores the product owner's output as the next spec revision (once per job), adds the spec as a dependent of the assumptions the job stated, renders it under the goal's text in the goal issue, moves the goal to `spec_review`. `summary` is the text of the `spec_approval` gate. |
| `goal.plan` | `goal_id`, `spec_id`, `platforms`, `plan`, `spec` | `{valid, errors, tasks: [{id, ref, number, seq, title, issue_number, branch, needs, criteria}]}`. Stores the architect's tasks in order, one issue per task (labels `forecrew:task` and `needs:<label>`, a sub-issue of the goal's), comments the plan on the goal issue and moves the goal to `planned`. `needs` are the task's labels plus `platforms`. A plan that cannot be used (the architect escalated, no task, an unknown criterion, a dependency that is not an earlier task, criteria of the spec that no task covers) stores nothing: the output is `valid: false` with `errors` as the architect's feedback, which is also commented on the goal issue (once per call) for the human a `needs_you` gate asks. Kept tasks and issues are not created twice. |
| `goal.round` | `task_id`, `iteration`, `human`, `review` | `{task_id, ref, title, branch, feedback}`. Before a developer job: the task goes (back) to `in_progress`, the goal to `in_progress`, the branch `agent/TASK-n` is recorded. `feedback` is `human` in the first round and `review` in the others. |
| `goal.task_move` | `task_id`, `to` | `{status}`. Moves a task to a state, through the ones in between when going forward, or back to `in_progress`. |
| `goal.pr_text` | `task_id`, `result` | `{ready, title, body}`: the PR text from the developer's output, the task with its criteria, the assumptions (the register's for the task, with refs and status, else the developer's) and `Closes #<task issue>`. When the developer reports an escalation it comments the reason on the task's issue and outputs `ready: false`. |
| `goal.review_post` | `repo`, `number`, `review` | `{posted}`. Posts the reviewer's output as a `COMMENT` review (never an approval or a request for changes): comments on a line are inline, the rest goes into the body with the verdict and the criteria check. Posts once per call key. |
| `goal.assess` | `repo`, `number`, `checks`, `review`, `result`, `sha`, optional `task_id`, `job_id` | `{done, merged, if_ready, feedback, gate_summary}`. Looks at the PR again: merged on GitHub is `done` and `merged`; closed there fails. Failing checks or a verdict other than `approve` give the rework `feedback`. With `task_id` it also reads the assumption register: a high-impact assumption the task rests on that is still `unverified`, or one invalidated after the developer job `job_id` finished, is not `done` and its `feedback` names the assumptions (statements as untrusted data). Otherwise `done`, `if_ready: [number]` and the `merge_approval` gate's `gate_summary` (checks, verdict, criteria, the task's high-impact assumptions with their status, its open medium-impact ones) for the commit `sha` the developer pushed. |
| `goal.hold` | `task_id`, `repo`, `number` | `{gates, assumptions, clear}`: the IDs of the pending `assumption_review` gates, and the refs, of the high-impact assumptions the task rests on that are still unverified; `clear` when no review is pending or the PR was merged or closed on GitHub. Read-only. A `loop` until `clear` runs it, with a `foreach` of `wait` steps on `gate.resolved` (`timeout` 72h, `on_failure: escalate`), so the run waits for a human to accept or reject them instead of spending rework rounds, and again after a human asked for changes (that opens a new review). |
| `goal.if_approved` | `decision`, `number` | `{if_approved}`: `[number]` for `approved`, else `[]`. |
| `goal.task_done` | `task_id`, `repo`, `number`, `feedback` | `{done, merged, sha, feedback}`. A merged PR moves the task to `merged` (`done`); otherwise it outputs the human's `feedback` for the next delivery; closed without merging fails. |
| `goal.close` | `goal_id` | `{tasks, cost_usd}`. Needs every task merged. Comments the summary (tasks, PRs, developer runs, cost of every job of the goal) on the goal issue, closes it and moves the goal to `done`. |
| `goal.settle` | `goal_id` | `{status}`. Cancels a goal that is not done, closes its unmerged tasks and their issues and the goal's with a comment (once). A task whose PR is merged, as the change or GitHub say, becomes `merged`, not `closed`: merged PRs stay merged. |
| `tests.locked_paths` | `task_id` | `{locked_paths}`. The files of every successful `test-engineer` job of the task (`files` and the `file` of every test in its `test-map.json` output; a retry lists only what it changed itself, so all jobs count), as globs that match exactly those paths (`[`, `*`, `?`, `{`, `}` become one-character classes), sorted; `[]` when the task has no such job. Fails for a file name with a backslash or starting with `!`. Pass it as `with.locked_paths` of the developer's `job` step; the JobCreator refuses control characters and backticks in it. |
| `tests.base_check_request` | `repo`, `base`, `head`, `tests`, optional `files` | `{requests}`. `tests` are the `tests` entries of the test map (`suite`, `id`, `file`). Reads `.forecrew/matrix.yaml` and the environment files at `head`. For every suite the tests belong to and every environment its cells run in, one command job request (the first cell that maps to that environment) that runs the suite on `base` with `overlay` = `head` and the files of `files` and of `tests` together. Only test files may be overlaid: a path the `test-engineer` role may not write (`Makefile`, `go.mod`, `.forecrew/**`, scripts) fails the action. Fails when `head` has no valid matrix or environment, or names no such suite. Use the requests as the `items` of a `foreach` of `kind: command` job steps. |
| `tests.base_check_result` | `runs`, `tests` | `{new_tests: [{id, failed_on_base}], ok}`. `runs` are the base check's jobs: job IDs, the outputs of job steps (`job_id`), or the rounds of a `foreach` over a job step (`{<step id>: {job_id, ...}}`); an element without a job ID fails the action. `tests` are the same entries as for the request. Reads the test runs the jobs stored. A test failed on base when, in every run of its suite, it has a result and all its results (a repeated id has several) are `failed` or `errored`, or it has no result and the command really failed: `exit_code` above 0 and the run is `errored`, has a `(command)` result, or is not `passed` (a compile error of one package next to an unrelated failing test counts). It does not count when the exit code is 0 (wrong report path, unreadable or empty report), -1 (timeout or not started), or the run is an oversized one (`max_test_results_size`). A test that passed or was skipped, and one whose suite has no run, did not fail on base. `ok` is true when every new test failed on base. |
| `profile.validate` | `profile` and/or `repo` and `ref`, `attempt`, `attempts` | `{valid, errors}`: checks the profile value and/or the profile file of `repo` at `ref`. Fails on the last attempt. |
| `discover.github` | `repo` | `{integrations, notes, text}`. Reads, with the App's token, the repo's autolinks and the link hosts in the bodies of its 100 most recent issues and pull requests, and outputs the integrations found (confidence and evidence as in the profile). It never contacts the hosts it finds. A read the App may not do is a note, not a failure; `text` is what the analyst is told, empty when nothing was found. Read-only, so idempotent. Use `on_failure: escalate`. |
| `matrix.plan` | `repo`, `ref`, `scope`, `suites` | `{cells, jobs, errors}`. Reads `.forecrew/matrix.yaml` and the environments its rules name at `ref` and expands them (see [Running a matrix](#running-a-matrix)). Invalid files, a scope that does not fit the matrix, or a matrix over the plan's limits give `errors` as text and empty `cells` and `jobs`, for the playbook to act on; only a read that fails fails the step. Read-only, so idempotent. |
| `profile.feedback` | `human`, `errors`, `discovery` | The feedback text of an analyst job: the reviewer's feedback, the validator's errors and the `discovery` text, each when not empty. |
| `onboarding.proposal` | `profile`, `summary`, `assumptions` | `{title, body}`: the profile PR text. |
| `onboarding.settle` | none | Puts a project still onboarding back to registered. |
| `onboarding.complete` | `repo`, `sha`, `ref` | Reads `.forecrew/profile.yaml` at the merge commit `sha`, or at `ref` when `sha` is empty (GitHub merged the PR), validates and stores it, and moves the project to onboarded. |
| `knowledge.seed` | none | `{indexed, sections, curate, text, error}`. Indexes the project's repo knowledge (the mapped paths of its policy, or the default mapping) and outputs the seed sections (`architecture`, `conventions`, `testing`, `runbooks`, `glossary`) that no location covers. `curate` has one item when there are such sections and none otherwise, so a `foreach` over it skips the curation; `text` tells the curator what to fill and what already has a home. A repo that cannot be read, or an installation without a knowledge base, gives `error` and an empty `curate`: onboarding does not depend on it. |

### Command jobs

A `job` step with `kind: command` needs `command` instead of `role` and runs
a command without an agent. Its `with` keys are `kind`, `command` (`argv` or
`shell`, `env`, `workdir`, `reports`, `artifacts`), `environment`, `cell`,
`suite`, `overlay` (`ref`, `paths`), `base_ref`, `branch`, `needs`, `timeout`
(default 30m) and the goal, spec and task refs. `role`, `pool`, `prompt` and
`feedback` are refused: the pool follows from the environment. Its output is
empty; the results are the job's test runs (`list_test_runs`).

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
runs again with `locked_paths` empty for that round (the playbook passes `[]`
for the round the gate decision allows), and the lock is back in the next
rounds. Without approval the lock stays. The goal playbook wires this (T29).

Limit: the lock covers the files the test engineer wrote. A locked test can
still be neutralised without touching it, for example by a new `conftest.py`
or a change of the runner's configuration. The goal playbook must therefore
treat a new test that is missing from the results at `head` as a failure
(T29), and the reviewer sees the diff.

The fail-on-base check shows every new test fails without the change: the new
test files from `head` are run on `base` (`tests.base_check_request`) and
`tests.base_check_result` reads the stored runs.

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
| `knowledge-curator` | `knowledge/**` | `knowledge-seed.json`: summary, the entries it wrote with their sources, the sections it skipped, assumptions | profile |
| `test-engineer` | test files only: `**/*_test.*`, `**/*.test.*`, `**/test_*.*`, `*.spec.*` and `*_spec.*` with code extensions, and `tests/`, `test/`, `__tests__/` and `testdata/` directories; never `.forecrew/**`, `.github/**`, `vendor/`, `node_modules/`, nor dependency, lock, build, Docker, script or CI files at any depth (`go.mod`, `package.json`, `*.lock`, `Makefile`, `Dockerfile`, `*.sh`, ...) | `test-map.json`: per test its criterion, suite, JUnit id (`<classname>::<name>`) and file; every file created or changed (`files`, the paths later locked for the developer and applied on the base); untestable criteria with reasons; assumptions | profile, goal, spec, task with its criteria, test plan |

Every output also has `escalation` (null unless the role stopped for a
reason its `escalate_when` lists). The test plan section holds
`.forecrew/matrix.yaml`, `.forecrew/test-strategy.md` and every environment
file the matrix's rules name, read from GitHub at the job branch when the
matrix is there and at the base otherwise; files not written yet are marked
missing. The test roles are not used by a playbook yet.

The council roles (`facilitator`, `devils-advocate`, `market-researcher`,
`security-reviewer`, `ux`) and the council packs that seat them, with other
roles in a council mode, are described in [councils.md](councils.md). They
are not used by a playbook yet.

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
its `finally` step puts the project back to registered.

After `onboarding.complete` the playbook seeds the knowledge base
([knowledge.md](knowledge.md)): `knowledge.seed`, then, only when some seed
section has no location, a `knowledge-curator` job on `agent/kb-seed` that
proposes entries for those sections from `CLAUDE.md`, `AGENTS.md`, the README
and `docs/`, a PR, and a `knowledge_proposal` gate on that PR. Approving it
merges the PR; merging it on GitHub approves it and closing it rejects it
(which ends the run failed; the project stays onboarded). The curator role needs
an entry under `roles` in forecrew.yaml; without one the job fails and a
`needs_you` gate opens.

## The goal playbook

`packs/playbooks/goal.yaml` takes `repo`, `goal_id` and `platforms` (the
goal's worker labels); `create_goal` starts it.

Sizing: the `triage` role and `goal.size` decide which councils a goal gets
(config.md, sizing). A goal created with `councils` (`product`, `technical`,
or `[]` for none) keeps that size and skips the triage job. The goal playbook
does not run triage yet; wiring it in, with a `foreach` over `if_product` and
`if_technical`, is a later task.

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
