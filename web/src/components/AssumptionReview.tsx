import type { Assumption } from "@/api/types"
import { StatusBadge } from "@/components/StatusBadge"
import { levelTone } from "@/lib/format"

/** The assumption an assumption_review gate asks about: what is taken as true, how much rides on it and what depends on it. */
export function AssumptionDetails({ assumption: a }: { assumption: Assumption }) {
  return (
    <div className="space-y-2 text-sm" aria-label={`Assumption ${a.ref}`} role="group">
      <p className="text-base">
        <span className="font-mono">{a.ref}</span> {a.statement}
      </p>
      <p className="flex flex-wrap items-center gap-2">
        <StatusBadge tone={levelTone(a.impact)}>impact {a.impact}</StatusBadge>
        <StatusBadge>confidence {a.confidence}</StatusBadge>
        <StatusBadge>check by {a.verify_by}</StatusBadge>
        <StatusBadge tone="attention">{a.status}</StatusBadge>
      </p>
      <p>
        <span className="text-muted-foreground">Source:</span> <span className="font-mono">{a.source || "unknown"}</span>, made by {a.made_by}
      </p>
      <div>
        <p className="text-muted-foreground">Rests on it:</p>
        {a.dependents.length ? (
          <ul className="list-disc pl-5">
            {a.dependents.map((d, i) => (
              <li key={i}>
                {d.kind} <span className="font-mono">{d.id}</span>
              </li>
            ))}
          </ul>
        ) : (
          <p>Nothing yet.</p>
        )}
      </div>
      {a.evidence ? (
        <p>
          <span className="text-muted-foreground">Evidence:</span> {a.evidence}
        </p>
      ) : null}
    </div>
  )
}
