import { Link, useParams } from "@tanstack/react-router"
import { useGoal, useGoalJobs, useWorkers } from "@/api/queries"
import type { GoalDetail } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { ExternalLink } from "@/components/ExternalLink"
import { Markdown } from "@/components/Markdown"
import { GateList } from "@/components/GateList"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cost, gateKindLabel, goalTone, jobRef, jobTone, stageOf, taskTone, words, type Tone } from "@/lib/format"

const CHECKS: Record<string, { label: string; tone: Tone }> = {
  success: { label: "checks passing", tone: "good" },
  pending: { label: "checks running", tone: "attention" },
  failure: { label: "checks failing", tone: "bad" },
  none: { label: "no checks", tone: "neutral" },
  unknown: { label: "checks unknown", tone: "neutral" },
}

function TaskRow({ task }: { task: GoalDetail["tasks"][number] }) {
  return (
    <TableRow>
      <TableCell>
        <span className="font-mono">{task.ref}</span> {task.title}
      </TableCell>
      <TableCell>
        <StatusBadge tone={taskTone(task.status)}>{words(task.status)}</StatusBadge>
      </TableCell>
      <TableCell>
        {task.needs.length ? (
          <StatusBadge tone="attention">needs {task.needs.join(", ")}</StatusBadge>
        ) : (
          <span className="text-muted-foreground">nothing</span>
        )}
      </TableCell>
      <TableCell>{task.url ? <ExternalLink href={task.url}>Issue</ExternalLink> : <span className="text-muted-foreground">none</span>}</TableCell>
      <TableCell>
        {task.changes.length ? (
          <ul className="space-y-1">
            {task.changes.map((c) => (
              <li key={c.number} className="flex flex-wrap items-center gap-2">
                <ExternalLink href={c.url}>PR #{c.number}</ExternalLink>
                <span className="text-sm text-muted-foreground">{c.state}</span>
                {c.checks ? <StatusBadge tone={CHECKS[c.checks]?.tone}>{CHECKS[c.checks]?.label ?? c.checks}</StatusBadge> : null}
              </li>
            ))}
          </ul>
        ) : (
          <span className="text-muted-foreground">none</span>
        )}
      </TableCell>
      <TableCell className="text-right">{task.iterations}</TableCell>
      <TableCell className="text-right">{cost(task.cost_usd)}</TableCell>
    </TableRow>
  )
}

const pendingGates = (d: GoalDetail) => d.gates.filter((g) => g.status === "pending")
const hasPending = (d: GoalDetail) => pendingGates(d).length > 0

function GoalJobs({ goal, tasks }: { goal: string; tasks: GoalDetail["tasks"] }) {
  const jobs = useGoalJobs(goal)
  const workers = useWorkers()
  const taskRefs = new Map(tasks.map((t) => [t.id, t.ref]))
  const workerNames = new Map(workers.data?.map((w) => [w.id, w.name]))
  return (
    <Async query={jobs} isEmpty={(d) => d.length === 0} empty="No jobs yet.">
      {(items) => (
        <TableWrap label="Jobs">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Job</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Task</TableHead>
                <TableHead>Worker</TableHead>
                <TableHead className="text-right">Cost</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {items.map((j) => (
                <TableRow key={j.id}>
                  <TableCell>
                    <Link to="/jobs/$ref" params={{ ref: jobRef(j.number) }} className="inline-flex min-h-11 items-center gap-2 underline-offset-2 hover:underline">
                      <span className="font-mono">{jobRef(j.number)}</span> {j.role}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <StatusBadge tone={jobTone(j.status)}>{j.status}</StatusBadge>
                  </TableCell>
                  <TableCell className="font-mono">{(j.task_id && taskRefs.get(j.task_id)) || <span className="font-sans text-muted-foreground">none</span>}</TableCell>
                  <TableCell className="font-mono">{(j.worker_id && workerNames.get(j.worker_id)) || <span className="font-sans text-muted-foreground">none</span>}</TableCell>
                  <TableCell className="text-right">{cost(j.cost_usd)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableWrap>
      )}
    </Async>
  )
}

export function GoalPage() {
  const { ref } = useParams({ from: "/app/goals/$ref" })
  const goal = useGoal(ref)
  return (
    <Async query={goal}>
      {(d) => (
        <>
          <PageHeader title={`${d.ref} · ${d.goal.title}`} />
          <div className="mb-6 flex flex-wrap items-center gap-3 text-sm">
            <StatusBadge tone={goalTone(d.goal.status)}>{words(d.goal.status)}</StatusBadge>
            <span>Stage: {stageOf(d.goal.status)}</span>
            <Link to="/projects/$name" params={{ name: d.project }} className="inline-flex min-h-11 items-center font-mono underline underline-offset-2">
              {d.project}
            </Link>
            {d.goal.platforms?.length ? <span>Platforms: {d.goal.platforms.join(", ")}</span> : null}
            {d.url ? <ExternalLink href={d.url}>Goal issue</ExternalLink> : null}
            <span className="ml-auto font-medium">Total cost {cost(d.cost_usd)}</span>
          </div>

          {d.goal.body ? (
            <section aria-labelledby="description" className="mb-8">
              <h2 id="description" className="mb-2 text-lg font-semibold">
                Description
              </h2>
              <div className="rounded-xl border bg-card p-4">
                <Markdown>{d.goal.body}</Markdown>
              </div>
            </section>
          ) : null}

          <section aria-labelledby="spec" className="mb-8">
            <h2 id="spec" className="mb-2 text-lg font-semibold">
              Spec{d.spec ? <span className="ml-2 text-sm font-normal text-muted-foreground">revision {d.spec.revision}</span> : null}
            </h2>
            {d.spec ? (
              <div className="rounded-xl border bg-card p-4">
                <Markdown>{d.spec.body}</Markdown>
              </div>
            ) : (
              <p className="text-muted-foreground">No spec yet. It is written once the goal starts.</p>
            )}
          </section>

          <section aria-labelledby="tasks" className="mb-8">
            <h2 id="tasks" className="mb-2 text-lg font-semibold">
              Tasks
            </h2>
            {d.tasks.length ? (
              <TableWrap label="Tasks">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Task</TableHead>
                      <TableHead>State</TableHead>
                      <TableHead>Needs</TableHead>
                      <TableHead>Issue</TableHead>
                      <TableHead>Pull request</TableHead>
                      <TableHead className="text-right">Iterations</TableHead>
                      <TableHead className="text-right">Cost</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {d.tasks.map((t) => (
                      <TaskRow key={t.ref} task={t} />
                    ))}
                  </TableBody>
                </Table>
              </TableWrap>
            ) : (
              <p className="text-muted-foreground">No tasks yet.</p>
            )}
          </section>

          <section aria-labelledby="goal-jobs" className="mb-8">
            <h2 id="goal-jobs" className="mb-2 text-lg font-semibold">
              Jobs
            </h2>
            <GoalJobs goal={d.ref} tasks={d.tasks} />
          </section>

          {hasPending(d) ? (
            <section aria-labelledby="waiting" className="mb-8">
              <h2 id="waiting" className="mb-2 text-lg font-semibold">
                Waiting for you
              </h2>
              <GateList gates={pendingGates(d)} />
            </section>
          ) : null}

          <section aria-labelledby="gates">
            <h2 id="gates" className="mb-2 text-lg font-semibold">
              Gates
            </h2>
            {d.gates.length ? (
              <ul className="space-y-2">
                {d.gates.map((g) => (
                  <li key={g.ref} className="rounded-xl border bg-card p-3 text-sm">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-mono">{g.ref}</span>
                      <span>{gateKindLabel(g.kind)}</span>
                      <StatusBadge tone={g.status === "pending" ? "attention" : g.status === "approved" ? "good" : "neutral"}>{words(g.status)}</StatusBadge>
                      {g.resolved_by ? <span className="text-muted-foreground">by {g.resolved_by}</span> : null}
                      <span className="text-muted-foreground">
                        <Age iso={g.resolved_at ?? g.created_at} ago />
                      </span>
                    </div>
                    {g.feedback ? <p className="mt-2 whitespace-pre-wrap">{g.feedback}</p> : null}
                  </li>
                ))}
              </ul>
            ) : (
              <p className="text-muted-foreground">No gates.</p>
            )}
          </section>
        </>
      )}
    </Async>
  )
}
