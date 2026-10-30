import { Link } from "@tanstack/react-router"
import { useGates, useGoals, useJobs, useWorkers } from "@/api/queries"
import { Age } from "@/components/Age"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { Tile } from "@/components/Tile"
import { goalRef, jobRef, stageOf } from "@/lib/format"

function LoadError({ message }: { message: string }) {
  return <p className="text-sm text-destructive">Could not load: {message}</p>
}

function Decisions() {
  const gates = useGates()
  if (gates.isError) return <LoadError message={gates.error.message} />
  if (!gates.data) return <p className="text-sm text-muted-foreground">Loading…</p>
  const n = gates.data.length
  const oldest = n ? gates.data.reduce((a, b) => (Date.parse(a.created_at) <= Date.parse(b.created_at) ? a : b)) : undefined
  return (
    <>
      <p className="text-3xl font-semibold">
        {n === 0 ? "Nothing needs you" : n === 1 ? "1 decision needs you" : `${n} decisions need you`}
      </p>
      {oldest ? (
        <p className="text-sm text-muted-foreground">
          Oldest waiting <Age iso={oldest.created_at} />
        </p>
      ) : (
        <StatusBadge tone="good">All clear</StatusBadge>
      )}
    </>
  )
}

function ActiveGoals() {
  const goals = useGoals()
  if (goals.isError) return <LoadError message={goals.error.message} />
  if (!goals.data) return <p className="text-sm text-muted-foreground">Loading…</p>
  const active = goals.data.filter((g) => g.status !== "done" && g.status !== "canceled")
  if (!active.length) return <p className="text-muted-foreground">No active goals</p>
  return (
    <>
      <p className="text-3xl font-semibold">{active.length} active</p>
      <ul className="space-y-2">
        {active.slice(0, 5).map((g) => (
          <li key={g.id} className="flex items-center justify-between gap-2 text-sm">
            <Link to="/goals/$ref" params={{ ref: goalRef(g.number) }} className="min-w-0 truncate underline-offset-2 hover:underline">
              <span className="font-mono">{goalRef(g.number)}</span> {g.title}
            </Link>
            <StatusBadge>{stageOf(g.status)}</StatusBadge>
          </li>
        ))}
      </ul>
    </>
  )
}

function RunningJobs() {
  const jobs = useJobs(undefined, "running")
  if (jobs.isError) return <LoadError message={jobs.error.message} />
  if (!jobs.data) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!jobs.data.length) return <p className="text-muted-foreground">No jobs running</p>
  return (
    <>
      <p className="text-3xl font-semibold">{jobs.data.length} running</p>
      <ul className="space-y-1 text-sm">
        {jobs.data.slice(0, 5).map((j) => (
          <li key={j.id}>
            <Link to="/jobs/$ref" params={{ ref: jobRef(j.number) }} className="underline-offset-2 hover:underline">
              <span className="font-mono">{jobRef(j.number)}</span> {j.role}
            </Link>
          </li>
        ))}
      </ul>
    </>
  )
}

function WorkersOnline() {
  const workers = useWorkers()
  if (workers.isError) return <LoadError message={workers.error.message} />
  if (!workers.data) return <p className="text-sm text-muted-foreground">Loading…</p>
  const online = workers.data.filter((w) => w.online).length
  const total = workers.data.length
  return (
    <>
      <p className="text-3xl font-semibold">
        {online} of {total} online
      </p>
      {total === 0 ? (
        <StatusBadge tone="attention">No workers</StatusBadge>
      ) : online === total ? (
        <StatusBadge tone="good">Healthy</StatusBadge>
      ) : online === 0 ? (
        <StatusBadge tone="bad">All offline</StatusBadge>
      ) : (
        <StatusBadge tone="attention">Some offline</StatusBadge>
      )}
    </>
  )
}

export function HomePage() {
  return (
    <>
      <PageHeader title="Home" />
      <div className="grid gap-4 sm:grid-cols-2">
        <Tile title="Decisions" to="/inbox" linkLabel="Open the inbox">
          <Decisions />
        </Tile>
        <Tile title="Goals" to="/goals" linkLabel="See all goals">
          <ActiveGoals />
        </Tile>
        <Tile title="Jobs" to="/jobs" search={{ status: "running" }} linkLabel="See running jobs">
          <RunningJobs />
        </Tile>
        <Tile title="Workers" to="/workers" linkLabel="See workers">
          <WorkersOnline />
        </Tile>
      </div>
    </>
  )
}
