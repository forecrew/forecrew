import { useQueryClient } from "@tanstack/react-query"
import { Link, useParams } from "@tanstack/react-router"
import { useEffect, useLayoutEffect, useRef, useState } from "react"
import { keys, useGoal, useJob, useJobLog } from "@/api/queries"
import type { JobEvent, JobInfo } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { ExternalLink } from "@/components/ExternalLink"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { CancelJob, cancelable } from "@/components/CancelJob"
import { count, cost, jobTone } from "@/lib/format"

const USAGE_PARTS: [string, string][] = [
  ["input_tokens", "input"],
  ["output_tokens", "output"],
  ["cache_read_tokens", "cache read"],
  ["cache_write_tokens", "cache write"],
]

/** A usage event as "tokens: 5 input, 2 output · $0.01". */
function usageText(data: Record<string, unknown> | undefined): string {
  const parts = USAGE_PARTS.filter(([k]) => typeof data?.[k] === "number").map(([k, label]) => `${count(data?.[k] as number)} ${label}`)
  const text = parts.length ? `tokens: ${parts.join(", ")}` : "tokens: none"
  return typeof data?.cost_usd === "number" ? `${text} · ${cost(data.cost_usd)}` : text
}

/** One event as plain text: untrusted output is never parsed as HTML. */
export function eventText(e: JobEvent): string {
  const data = e.data as Record<string, unknown> | undefined
  switch (e.type) {
    case "tool_call":
      return `${String(data?.name ?? "tool")} ${JSON.stringify(data?.input ?? {})}`
    case "tool_result":
      return `${String(data?.name ?? "tool")} ${data?.is_error ? "failed" : "returned"}: ${String(data?.output ?? "")}`
    case "usage":
      return usageText(data)
    default:
      return e.text ?? ""
  }
}

function EventLog({ events, running }: { events: JobEvent[]; running: boolean }) {
  const box = useRef<HTMLDivElement>(null)
  const [follow, setFollow] = useState(true)

  useLayoutEffect(() => {
    const el = box.current
    if (follow && el) el.scrollTop = el.scrollHeight
  }, [events.length, follow])

  return (
    <div className="relative">
      <div
        ref={box}
        role="log"
        aria-live="off"
        aria-label="Job log"
        tabIndex={0}
        onScroll={(e) => {
          const el = e.currentTarget
          setFollow(el.scrollHeight - el.scrollTop - el.clientHeight < 24)
        }}
        className="max-h-[28rem] overflow-auto rounded-xl border bg-card p-3 font-mono text-xs leading-relaxed"
      >
        {events.length === 0 ? <p className="text-muted-foreground">{running ? "Waiting for the first event…" : "No events."}</p> : null}
        {events.map((e) => (
          <div key={e.seq} className="whitespace-pre-wrap break-words">
            <span className="text-label">{String(e.seq).padStart(3, "0")}</span>{" "}
            <span className={e.type === "error" ? "font-semibold text-danger" : "font-semibold"}>{e.type}</span> {eventText(e)}
          </div>
        ))}
      </div>
      {!follow ? (
        <Button variant="secondary" className="absolute right-3 bottom-3" onClick={() => setFollow(true)}>
          Follow new events
        </Button>
      ) : null}
    </div>
  )
}

/** The goal and task a job works on, as links: the job only holds their IDs. */
function GoalTask({ job }: { job: JobInfo }) {
  const goal = useGoal(job.goal_id ?? "", Boolean(job.goal_id))
  if (!job.goal_id) return null
  const ref = goal.data?.ref
  const task = job.task_id ? goal.data?.tasks.find((t) => t.id === job.task_id) : undefined
  return (
    <>
      <dt className="text-label">Goal</dt>
      <dd>
        <Link to="/goals/$ref" params={{ ref: ref ?? job.goal_id }} className="inline-flex min-h-11 items-center font-mono underline underline-offset-2">
          {ref ?? "Goal"}
        </Link>
      </dd>
      {job.task_id ? (
        <>
          <dt className="text-label">Task</dt>
          <dd>
            {task?.url ? (
              <ExternalLink href={task.url}>{task.ref}</ExternalLink>
            ) : (
              <span className="font-mono">{task?.ref ?? "unknown"}</span>
            )}
          </dd>
        </>
      ) : null}
    </>
  )
}

/** The output of a job as formatted JSON, folded away: it can be long. */
function Output({ output }: { output: unknown }) {
  return (
    <details className="rounded-xl border bg-card p-3 text-sm">
      <summary className="min-h-11 cursor-pointer py-2 font-medium">Output</summary>
      <pre tabIndex={0} aria-label="Output as JSON" className="max-h-96 overflow-auto font-mono text-xs break-words whitespace-pre-wrap">
        {JSON.stringify(output, null, 2)}
      </pre>
    </details>
  )
}

export function JobPage() {
  const { ref } = useParams({ from: "/app/jobs/$ref" })
  const qc = useQueryClient()
  const job = useJob(ref)
  const running = job.data?.status === "running"
  const log = useJobLog(ref, running)
  const status = job.data?.status

  // When a job ends, read the log one last time.
  useEffect(() => {
    if (status && status !== "running" && status !== "queued") void qc.invalidateQueries({ queryKey: keys.logs(ref) })
  }, [status, ref, qc])

  return (
    <Async query={job}>
      {(j) => (
        <>
          <PageHeader title={`${j.ref} · ${j.role}`}>{cancelable(j.status) ? <CancelJob jobRef={j.ref} /> : null}</PageHeader>
          <dl className="mb-6 grid gap-x-6 gap-y-2 rounded-xl border bg-card p-4 text-sm sm:grid-cols-[10rem_1fr]">
            <dt className="text-label">State</dt>
            <dd>
              <StatusBadge tone={jobTone(j.status)}>{j.status}</StatusBadge>
            </dd>
            {j.reason ? (
              <>
                <dt className="text-label">Reason</dt>
                <dd>{j.reason}</dd>
              </>
            ) : null}
            {j.needs.length ? (
              <>
                <dt className="text-label">Needs</dt>
                <dd className="flex flex-wrap gap-1">
                  {j.needs.map((n) => (
                    <StatusBadge key={n}>{n}</StatusBadge>
                  ))}
                </dd>
              </>
            ) : null}
            <dt className="text-label">Attempts</dt>
            <dd>
              {j.attempts} of {j.max_attempts}
            </dd>
            {j.status === "running" && j.lease_expires_at ? (
              <>
                <dt className="text-label">Lease expires</dt>
                <dd>
                  <Age iso={j.lease_expires_at} future />
                </dd>
              </>
            ) : null}
            {j.status === "queued" && j.run_after ? (
              <>
                <dt className="text-label">Retry</dt>
                <dd>
                  Waiting to retry <Age iso={j.run_after} future />
                </dd>
              </>
            ) : null}
            <dt className="text-label">Worker</dt>
            <dd className="font-mono">{j.worker_name ?? "none"}</dd>
            <dt className="text-label">Role</dt>
            <dd className="font-mono">{j.role}</dd>
            <dt className="text-label">Runtime</dt>
            <dd className="font-mono">{j.runtime}</dd>
            <dt className="text-label">Model</dt>
            <dd className="font-mono">{j.spec.model}</dd>
            <dt className="text-label">Repository</dt>
            <dd className="font-mono">
              {j.spec.repo}
              {j.spec.branch ? ` @ ${j.spec.branch}` : ""}
            </dd>
            {j.head_sha ? (
              <>
                <dt className="text-label">Commit</dt>
                <dd>
                  {j.spec.repo ? (
                    <ExternalLink href={`https://github.com/${j.spec.repo}/commit/${j.head_sha}`}>
                      <span className="font-mono">{j.head_sha.slice(0, 7)}</span>
                    </ExternalLink>
                  ) : (
                    <span className="font-mono">{j.head_sha.slice(0, 7)}</span>
                  )}
                </dd>
              </>
            ) : null}
            <GoalTask job={j} />
            <dt className="text-label">Cost</dt>
            <dd>{cost(j.cost_usd)}</dd>
            <dt className="text-label">Tokens</dt>
            <dd>
              {count(j.usage.input_tokens)} input, {count(j.usage.output_tokens)} output, {count(j.usage.cache_read_tokens)} cache read,{" "}
              {count(j.usage.cache_write_tokens)} cache write
            </dd>
            <dt className="text-label">Created</dt>
            <dd>
              <Age iso={j.created_at} ago />
            </dd>
            {j.finished_at ? (
              <>
                <dt className="text-label">Finished</dt>
                <dd>
                  <Age iso={j.finished_at} ago />
                </dd>
              </>
            ) : null}
          </dl>
          {j.output === undefined || j.output === null ? null : (
            <div className="mb-6">
              <Output output={j.output} />
            </div>
          )}
          <h2 className="mb-2 text-lg font-semibold">Log</h2>
          {log.isError ? (
            <p role="alert" className="text-sm text-danger">
              Could not load the log: {log.error.message}
            </p>
          ) : (
            <EventLog events={log.data?.events ?? []} running={running} />
          )}
        </>
      )}
    </Async>
  )
}
