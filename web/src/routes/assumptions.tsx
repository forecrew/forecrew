import { useNavigate, useSearch } from "@tanstack/react-router"
import { useAssumptionRegister, useProjects } from "@/api/queries"
import { ASSUMPTION_STATUSES, LEVELS, type Assumption, type AssumptionStatus, type Level } from "@/api/types"
import { Age } from "@/components/Age"
import { AssumptionResolve } from "@/components/AssumptionResolve"
import { Async } from "@/components/Async"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { WhyLink } from "@/components/WhyLink"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import type { Tone } from "@/lib/format"

export const assumptionTone = (s: AssumptionStatus): Tone => (s === "verified" ? "good" : s === "invalidated" ? "bad" : s === "unverified" ? "attention" : "neutral")

function Item({ a, project }: { a: Assumption; project?: string }) {
  const dependents = a.dependents.map((d) => d.id)
  return (
    <article aria-labelledby={`${a.ref}-statement`} className="space-y-2 rounded-xl border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm">{a.ref}</span>
        <StatusBadge tone={assumptionTone(a.status)}>{a.status}</StatusBadge>
        <StatusBadge tone={a.impact === "high" ? "attention" : "neutral"}>{a.impact} impact</StatusBadge>
        {project ? <span className="font-mono text-sm text-muted-foreground">{project}</span> : null}
        <span className="ml-auto flex items-center gap-3 text-sm text-muted-foreground">
          <Age iso={a.created_at} ago />
          <WhyLink to={a.ref} />
        </span>
      </div>
      <h2 id={`${a.ref}-statement`} className="font-medium break-words whitespace-pre-wrap">
        {a.statement}
      </h2>
      <dl className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[9rem_1fr]">
        <dt className="text-muted-foreground">Confidence</dt>
        <dd>{a.confidence}</dd>
        <dt className="text-muted-foreground">Verify by</dt>
        <dd>{a.verify_by}</dd>
        <dt className="text-muted-foreground">Made by</dt>
        <dd className="break-words">
          {a.made_by}
          {a.source ? <span className="font-mono"> ({a.source})</span> : null}
        </dd>
        <dt className="text-muted-foreground">Rests on it</dt>
        <dd className="font-mono break-words">{dependents.length ? dependents.join(", ") : <span className="font-sans text-muted-foreground">nothing yet</span>}</dd>
        {a.evidence ? (
          <>
            <dt className="text-muted-foreground">Evidence</dt>
            <dd className="break-words whitespace-pre-wrap">
              {a.evidence}
              {a.resolved_by ? <span className="text-muted-foreground"> ({a.resolved_by})</span> : null}
            </dd>
          </>
        ) : null}
      </dl>
      <AssumptionResolve assumption={a} />
    </article>
  )
}

export function AssumptionsPage() {
  const { project = "", status = "", impact = "" } = useSearch({ from: "/app/assumptions" })
  const navigate = useNavigate({ from: "/assumptions" })
  const assumptions = useAssumptionRegister(project, status as AssumptionStatus | "", impact as Level | "")
  const projects = useProjects()
  const names = new Map(projects.data?.map((p) => [p.id, p.name]))
  const setFilter = (next: { status?: string; impact?: string }) =>
    void navigate({ search: (prev) => ({ ...prev, ...Object.fromEntries(Object.entries(next).map(([k, v]) => [k, v || undefined])) }) })

  return (
    <>
      <PageHeader title="Assumptions" meta="What agents took for true, and what rests on it. Resolve one here with its evidence." />
      <div className="mb-4 flex flex-wrap gap-4">
        <div className="space-y-1.5">
          <Label htmlFor="assumption-status">Status</Label>
          <NativeSelect id="assumption-status" className="w-full sm:w-48" value={status} onChange={(e) => setFilter({ status: e.target.value })}>
            <NativeSelectOption value="">All statuses</NativeSelectOption>
            {ASSUMPTION_STATUSES.map((s) => (
              <NativeSelectOption key={s} value={s}>
                {s}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="assumption-impact">Impact</Label>
          <NativeSelect id="assumption-impact" className="w-full sm:w-48" value={impact} onChange={(e) => setFilter({ impact: e.target.value })}>
            <NativeSelectOption value="">Any impact</NativeSelectOption>
            {LEVELS.map((l) => (
              <NativeSelectOption key={l} value={l}>
                {l}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
      </div>
      <Async query={assumptions} isEmpty={(d) => d.length === 0} empty="No assumptions match.">
        {(items) => (
          <ul aria-label="Assumptions" className="space-y-3">
            {items.map((a) => (
              <li key={a.id}>
                <Item a={a} project={project ? undefined : names.get(a.project_id)} />
              </li>
            ))}
          </ul>
        )}
      </Async>
    </>
  )
}
