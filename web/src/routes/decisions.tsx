import { Link, useSearch } from "@tanstack/react-router"
import { useDecisions, useProjects } from "@/api/queries"
import type { Decision } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { Card, CardContent, CardHeader } from "@/components/ui/card"

function DecisionCard({ decision: d, project }: { decision: Decision; project?: string }) {
  const options = d.options ?? []
  return (
    <Card>
      <CardHeader className="flex flex-row flex-wrap items-center gap-2">
        <span className="font-mono font-medium">{d.ref}</span>
        <h2 className="min-w-0 flex-1 font-medium">{d.question}</h2>
        <StatusBadge tone={d.decided_by === "policy" ? "attention" : "neutral"}>{d.decided_by === "policy" ? "by policy" : `by ${d.decided_by}`}</StatusBadge>
        <span className="text-sm text-muted-foreground">
          <Age iso={d.created_at} ago />
        </span>
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        <p>
          Chose <strong className="font-mono">{d.choice}</strong>
          {project ? (
            <>
              {" "}
              in <span className="font-mono">{project}</span>
            </>
          ) : null}
          {options.length ? <span className="text-muted-foreground">, among {options.map((o) => o.name).join(", ")}</span> : null}
        </p>
        {d.reasons ? <p>{d.reasons}</p> : null}
        {d.dissent ? (
          <p>
            <span className="text-xs font-medium text-muted-foreground">Dissent</span>
            <br />
            {d.dissent}
          </p>
        ) : null}
        {d.assumptions.length ? (
          <p className="text-muted-foreground">
            Rests on <span className="font-mono">{d.assumptions.join(", ")}</span>
          </p>
        ) : null}
        <p className="flex flex-wrap gap-x-4">
          {d.council_id ? (
            <Link to="/councils/$ref" params={{ ref: d.council_id }} className="text-primary underline underline-offset-2">
              Open the council
            </Link>
          ) : (
            <span className="text-muted-foreground">Not decided by a council.</span>
          )}
          {d.goal_id ? (
            <Link to="/goals/$ref" params={{ ref: d.goal_id }} hash="gates" className="text-primary underline underline-offset-2">
              Open its gate on the goal
            </Link>
          ) : null}
        </p>
      </CardContent>
    </Card>
  )
}

/** The decisions register, newest first. The project comes from the sidebar switcher. */
export function DecisionsPage() {
  const { project = "" } = useSearch({ from: "/app/decisions" })
  const decisions = useDecisions(project)
  const projects = useProjects()
  const names = new Map(projects.data?.map((p) => [p.id, p.name]))
  return (
    <>
      <PageHeader title="Decisions" meta="What was decided, by whom, and what it rests on." />
      <Async query={decisions} isEmpty={(d) => d.length === 0} empty="No decisions yet. Approved specs, merges and councils are recorded here.">
        {(items) => (
          <ul className="space-y-4" aria-label="Decisions">
            {items.map((d) => (
              <li key={d.id}>
                <DecisionCard decision={d} project={project ? undefined : names.get(d.project_id)} />
              </li>
            ))}
          </ul>
        )}
      </Async>
    </>
  )
}
