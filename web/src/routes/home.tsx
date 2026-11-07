import { Link } from "@tanstack/react-router"
import { cn } from "cn"
import { SearchIcon } from "lucide-react"
import { useState } from "react"
import { useConnections, useFleet, useGates, useGoals, useJobs, useMembers, useProjects, useResources } from "@/api/queries"
import type { GoalStatus } from "@/api/types"
import { Age } from "@/components/Age"
import { useOpenCommandMenu } from "@/components/CommandMenu"
import { StatusBadge } from "@/components/StatusBadge"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { gateKindLabel, goalRef, jobRef, stageOf } from "@/lib/format"
import { useCan, useCanAnywhere } from "@/lib/permissions"

function LoadError({ message }: { message: string }) {
  return <p className="text-sm text-destructive">Could not load: {message}</p>
}

const Loading = () => <p className="text-sm text-muted-foreground">Loading…</p>

function Panel({ title, to, search, linkLabel, children, className }: {
  title: string
  to: string
  search?: Record<string, string>
  linkLabel: string
  children: React.ReactNode
  className?: string
}) {
  return (
    <Card className={className}>
      <CardHeader className="flex flex-row items-center justify-between gap-2 border-b">
        <h2 className="font-medium">{title}</h2>
        <Link to={to} search={search as never} className="text-sm text-primary underline underline-offset-2">
          {linkLabel}
        </Link>
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  )
}

function Stat({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Card>
      <CardHeader>
        <h2 className="text-sm font-medium text-muted-foreground">{title}</h2>
      </CardHeader>
      <CardContent className="space-y-1">{children}</CardContent>
    </Card>
  )
}

const plural = (n: number) => (n === 1 ? "1 decision needs you" : `${n} decisions need you`)

/** The date, and in one line how many decisions wait. */
function Headline() {
  const gates = useGates()
  const today = new Date().toLocaleDateString("en-GB", { weekday: "long", day: "numeric", month: "long" })
  const n = gates.data?.length
  return (
    <div className="mb-6">
      <p className="text-3xl font-semibold tracking-tight">{today}</p>
      <p className="text-muted-foreground">{n === undefined ? " " : n === 0 ? "Nothing needs you" : plural(n)}</p>
    </div>
  )
}

function NeedsYouStat() {
  const gates = useGates()
  if (gates.isError) return <LoadError message={gates.error.message} />
  if (!gates.data) return <Loading />
  const n = gates.data.length
  const oldest = n ? gates.data.reduce((a, b) => (Date.parse(a.created_at) <= Date.parse(b.created_at) ? a : b)) : undefined
  return (
    <>
      <p className="text-3xl font-semibold">{n}</p>
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

/** Improvement proposals waiting for a decision; a snoozed one has no gate on the list. */
function SuggestionsStat() {
  const gates = useGates()
  if (gates.isError) return <LoadError message={gates.error.message} />
  if (!gates.data) return <Loading />
  const n = gates.data.filter((g) => g.kind === "improvement_proposal").length
  return (
    <>
      <p className="text-3xl font-semibold">{n}</p>
      <Link to="/proposals" className="text-sm text-primary underline underline-offset-2">
        Open proposals
      </Link>
    </>
  )
}

function AgentsStat() {
  const jobs = useJobs(undefined, "running")
  if (jobs.isError) return <LoadError message={jobs.error.message} />
  if (!jobs.data) return <Loading />
  return (
    <>
      <p className="text-3xl font-semibold">{jobs.data.length}</p>
      <p className="text-sm text-muted-foreground">{jobs.data.length === 0 ? "No jobs running" : "jobs running"}</p>
    </>
  )
}

function MachinesStat() {
  const fleet = useFleet()
  if (fleet.isError) return <LoadError message={fleet.error.message} />
  if (!fleet.data) return <Loading />
  const { working, waiting, locations } = fleet.data
  return (
    <>
      <p className="text-3xl font-semibold">
        {working} working, {waiting} waiting
      </p>
      {locations.length === 0 ? (
        <StatusBadge tone="attention">No machines yet</StatusBadge>
      ) : waiting > 0 ? (
        <StatusBadge tone="attention">Jobs are waiting</StatusBadge>
      ) : (
        <StatusBadge tone="good">Nothing waits</StatusBadge>
      )}
    </>
  )
}

function NeedsYou() {
  const gates = useGates()
  return (
    <Panel title="Needs you" to="/inbox" linkLabel="Open inbox">
      {gates.isError ? (
        <LoadError message={gates.error.message} />
      ) : !gates.data ? (
        <Loading />
      ) : gates.data.length === 0 ? (
        <p className="text-muted-foreground">Nothing needs you</p>
      ) : (
        <ul className="divide-y">
          {gates.data.slice(0, 5).map((g) => (
            <li key={g.ref} className="flex items-center gap-3 py-3 first:pt-0 last:pb-0">
              <StatusBadge tone={g.kind === "needs_you" ? "attention" : "neutral"} className="shrink-0">
                {gateKindLabel(g.kind)}
              </StatusBadge>
              <div className="min-w-0 flex-1">
                <p className="truncate font-medium">{g.title}</p>
                <p className="truncate text-sm text-muted-foreground">
                  <span className="font-mono">{g.project}</span>
                  {g.goal_ref ? <> · {g.goal_ref}</> : null} · waiting <Age iso={g.created_at} />
                </p>
              </div>
              <Link to="/inbox" search={{ gate: g.ref }} aria-label={`${g.kind === "needs_you" ? "Decide" : "Review"} ${g.ref}`} className={cn(buttonVariants({ variant: "outline" }))}>
                {g.kind === "needs_you" ? "Decide" : "Review"}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}

const STAGES = ["Spec", "Plan", "Build", "Done"] as const
const STAGE_INDEX: Record<GoalStatus, number> = { draft: 0, spec_review: 0, planned: 1, in_progress: 2, done: 3, canceled: 0 }

/** Four segments, filled up to the goal's stage. The word next to it says the same. */
export function StageBar({ status }: { status: GoalStatus }) {
  const at = STAGE_INDEX[status]
  return (
    <span aria-hidden="true" className="order-last flex w-32 basis-full gap-1 sm:order-none sm:basis-auto">
      {STAGES.map((s, i) => (
        <span key={s} className={`h-1.5 flex-1 rounded-full ${i <= at ? "bg-primary" : "bg-muted"}`} />
      ))}
    </span>
  )
}

function ActiveGoals() {
  const goals = useGoals()
  const active = goals.data?.filter((g) => g.status !== "done" && g.status !== "canceled")
  return (
    <Panel title="Active goals" to="/goals" linkLabel="See all goals">
      {goals.isError ? (
        <LoadError message={goals.error.message} />
      ) : !active ? (
        <Loading />
      ) : active.length === 0 ? (
        <p className="text-muted-foreground">No active goals</p>
      ) : (
        <ul className="divide-y">
          {active.slice(0, 5).map((g) => (
            <li key={g.id} className="flex flex-wrap items-center gap-x-4 gap-y-1 py-3 first:pt-0 last:pb-0">
              <Link to="/goals/$ref" params={{ ref: goalRef(g.number) }} className="min-w-0 flex-1 truncate underline-offset-2 hover:underline">
                <span className="font-mono">{goalRef(g.number)}</span> {g.title}
              </Link>
              <StageBar status={g.status} />
              <StatusBadge>{stageOf(g.status)}</StatusBadge>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}

function LiveActivity() {
  const jobs = useJobs(undefined, "running")
  return (
    <Panel title="Live activity" to="/jobs" search={{ status: "running" }} linkLabel="Watch">
      {jobs.isError ? (
        <LoadError message={jobs.error.message} />
      ) : !jobs.data ? (
        <Loading />
      ) : jobs.data.length === 0 ? (
        <p className="text-muted-foreground">No jobs running</p>
      ) : (
        <ul className="divide-y">
          {jobs.data.slice(0, 5).map((j) => (
            <li key={j.id} className="py-3 first:pt-0 last:pb-0">
              <Link to="/jobs/$ref" params={{ ref: jobRef(j.number) }} className="underline-offset-2 hover:underline">
                <span className="font-mono">{jobRef(j.number)}</span> {j.role}
              </Link>
              <p className="text-sm text-muted-foreground">
                started <Age iso={j.started_at ?? j.created_at} ago />
              </p>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}

/** The locations, not one row per short-lived machine. */
function Fleet() {
  const fleet = useFleet()
  return (
    <Panel title="Fleet" to="/machines" linkLabel="See machines">
      {fleet.isError ? (
        <LoadError message={fleet.error.message} />
      ) : !fleet.data ? (
        <Loading />
      ) : fleet.data.locations.length === 0 ? (
        <p className="text-muted-foreground">No machines yet</p>
      ) : (
        <ul className="divide-y">
          {fleet.data.locations.map((l) => (
            <li key={l.id} className="flex items-center justify-between gap-2 py-2 first:pt-0 last:pb-0 text-sm">
              <span>{l.name}</span>
              {l.status === "online" ? (
                <span className="text-muted-foreground">{l.kind === "computer" ? l.in_use : l.capacity > 0 ? `${l.in_use} of ${l.capacity}` : l.in_use} {l.kind === "computer" ? "directly" : "machines"}</span>
              ) : (
                <StatusBadge tone="bad">{l.status}</StatusBadge>
              )}
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}

/** First run: nothing is connected yet, so say where to begin. Where jobs run comes first. */
function Welcome() {
  const fleet = useFleet()
  const projects = useProjects()
  const can = useCan()
  if (!can("worker.manage") || fleet.data?.locations.length !== 0 || projects.data?.length !== 0) return null
  return (
    <section aria-label="Welcome" className="mb-6 rounded-xl border bg-card p-4">
      <h2 className="font-medium">Welcome to Forecrew</h2>
      <p className="mb-2 text-sm text-muted-foreground">Agents need somewhere to work before they can do anything. Three decisions, and you are set.</p>
      <Link to="/setup" className={cn(buttonVariants())}>
        Set up Forecrew
      </Link>
    </section>
  )
}

const DISMISSED = "forecrew.setup.dismissed"
const dismissedItems = (): string[] => {
  try {
    return JSON.parse(localStorage.getItem(DISMISSED) ?? "[]") as string[]
  } catch {
    return []
  }
}

/** A short list of what to set up next. It shows only what applies to the person, and is gone for good once empty. */
function FinishSetup() {
  const can = useCan()
  const fleet = useFleet()
  const members = useMembers(can("member.manage"))
  const connections = useConnections(can("connection.manage"))
  const models = useResources("model_connection", can("models.manage"))
  const [dismissed, setDismissed] = useState(dismissedItems)
  const dismiss = (id: string) => {
    const next = [...dismissed, id]
    setDismissed(next)
    try {
      localStorage.setItem(DISMISSED, JSON.stringify(next))
    } catch {
      // the list comes back next time: nothing is lost
    }
  }
  const items = [
    { id: "team", text: "Invite your team", to: "/settings/people", show: can("member.manage") && members.data !== undefined && members.data.length < 2 },
    {
      id: "machine",
      text: "Add a Windows or Mac machine",
      to: "/machines/add",
      show: can("worker.manage") && fleet.data !== undefined && fleet.data.locations.length > 0 && !fleet.data.locations.some((l) => /windows|macos/i.test(l.runs)),
    },
    { id: "models", text: "Add an AI provider key", to: "/settings/models", show: can("models.manage") && models.data !== undefined && models.data.length === 0 },
    { id: "signin", text: "Connect company sign-in", to: "/settings/sign-in", show: can("connection.manage") && connections.data !== undefined && !connections.data.some((c) => !c.instance) },
  ].filter((i) => i.show && !dismissed.includes(i.id))
  if (items.length === 0) return null
  return (
    <section aria-label="Finish setting up" className="mb-6 rounded-xl border bg-card p-4">
      <h2 className="mb-2 font-medium">Finish setting up</h2>
      <ul className="space-y-1">
        {items.map((i) => (
          <li key={i.id} className="flex items-center justify-between gap-2 text-sm">
            <Link to={i.to} className="underline underline-offset-2">
              {i.text}
            </Link>
            <Button variant="ghost" size="sm" aria-label={`Dismiss: ${i.text}`} onClick={() => dismiss(i.id)}>
              Not now
            </Button>
          </li>
        ))}
      </ul>
    </section>
  )
}

export function HomePage() {
  const openCommands = useOpenCommandMenu()
  const canAnywhere = useCanAnywhere()
  return (
    <>
      <h1 className="sr-only">Home</h1>
      <div className="mb-6 flex items-center gap-2">
        <Button variant="outline" className="h-10 flex-1 justify-start px-3 font-normal text-muted-foreground" onClick={openCommands}>
          <SearchIcon aria-hidden="true" /> Search or run a command
        </Button>
        <kbd className="hidden rounded-md border px-1.5 py-1 font-mono text-xs text-muted-foreground sm:block">Ctrl K</kbd>
        {canAnywhere("goal.create") ? (
          <Link to="/goals/new" className={cn(buttonVariants({ size: "lg" }))}>
            New goal
          </Link>
        ) : null}
      </div>
      <Headline />
      <Welcome />
      <FinishSetup />
      <div className="mb-6 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat title="Decisions waiting">
          <NeedsYouStat />
        </Stat>
        <Stat title="Suggestions">
          <SuggestionsStat />
        </Stat>
        <Stat title="Agents working">
          <AgentsStat />
        </Stat>
        <Stat title="Machines">
          <MachinesStat />
        </Stat>
      </div>
      <div className="grid grid-cols-1 items-start gap-4 lg:grid-cols-[2fr_1fr]">
        <div className="space-y-4">
          <NeedsYou />
          <ActiveGoals />
        </div>
        <div className="space-y-4">
          <LiveActivity />
          <Fleet />
        </div>
      </div>
    </>
  )
}
