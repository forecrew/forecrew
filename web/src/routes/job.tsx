import { useQueryClient } from "@tanstack/react-query"
import { useParams } from "@tanstack/react-router"
import { useEffect, useLayoutEffect, useRef, useState } from "react"
import { keys, useJob, useJobLog } from "@/api/queries"
import type { JobEvent } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { cost, jobTone } from "@/lib/format"

/** One event as plain text: untrusted output is never parsed as HTML. */
export function eventText(e: JobEvent): string {
  const data = e.data as Record<string, unknown> | undefined
  switch (e.type) {
    case "tool_call":
      return `${String(data?.name ?? "tool")} ${JSON.stringify(data?.input ?? {})}`
    case "tool_result":
      return `${String(data?.name ?? "tool")} ${data?.is_error ? "failed" : "returned"}: ${String(data?.output ?? "")}`
    case "usage":
      return JSON.stringify(data ?? {})
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
          <PageHeader title={`${j.ref} · ${j.role}`} />
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
            <dt className="text-label">Attempts</dt>
            <dd>
              {j.attempts} of {j.max_attempts}
            </dd>
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
            <dt className="text-label">Cost</dt>
            <dd>{cost(j.cost_usd)}</dd>
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
