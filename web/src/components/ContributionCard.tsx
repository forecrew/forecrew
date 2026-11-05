import type { Contribution } from "@/api/types"
import { Age } from "@/components/Age"
import { StatusBadge } from "@/components/StatusBadge"
import { Card, CardContent, CardHeader } from "@/components/ui/card"

const isObject = (x: unknown): x is Record<string, unknown> => typeof x === "object" && x !== null && !Array.isArray(x)
const text = (x: unknown): string => (typeof x === "string" ? x : "")
const objects = (x: unknown): Record<string, unknown>[] => (Array.isArray(x) ? x.filter(isObject) : [])

function Field({ label, value }: { label: string; value: string }) {
  if (!value) return null
  return (
    <p>
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      <br />
      {value}
    </p>
  )
}

/** A proposal or a revision: the member's position and recommendation. */
function Proposal({ c }: { c: Record<string, unknown> }) {
  const options = objects(c.options).map((o) => text(o.name)).filter(Boolean)
  const revision = isObject(c.revision) ? c.revision : undefined
  return (
    <>
      <Field label="Priority" value={text(c.priority)} />
      <Field label="Position" value={text(c.position)} />
      <Field label="Recommends" value={text(c.recommendation)} />
      <Field label="Options" value={options.join(", ")} />
      {revision ? <Field label={revision.changed === true ? "Changed position because" : "Kept position because"} value={text(revision.reason)} /> : null}
    </>
  )
}

/** A critique: for every other member, what they disagree with. */
function Critique({ c }: { c: Record<string, unknown> }) {
  return (
    <>
      <Field label="Priority" value={text(c.priority)} />
      {objects(c.critiques).map((k, i) => (
        <div key={i}>
          <p className="text-xs font-medium text-muted-foreground">
            On <span className="font-mono">{text(k.member)}</span>
          </p>
          <ul className="list-disc pl-5">
            {objects(k.disagree).map((d, j) => (
              <li key={j}>
                {text(d.point)}: {text(d.reason)}
              </li>
            ))}
          </ul>
        </div>
      ))}
    </>
  )
}

/**
 * One member's contribution at one stage. Its content is JSON an agent wrote:
 * known fields are read defensively and shown as plain text, and the raw
 * output stays one click away for what the page does not know.
 */
export function ContributionCard({ contribution: k }: { contribution: Contribution }) {
  const checks = k.evidence_check ?? []
  const failed = checks.filter((s) => s.status !== "ok")
  const body = isObject(k.content) ? k.content : undefined
  return (
    <Card size="sm" className="h-full">
      <CardHeader className="flex flex-row flex-wrap items-center gap-2">
        <span className="font-mono font-medium">{k.member}</span>
        <span className="text-xs text-muted-foreground">
          <Age iso={k.created_at} ago />
        </span>
        {checks.length > 0 ? (
          <StatusBadge tone={failed.length ? "attention" : "good"}>
            {checks.length} sources{failed.length ? `, ${failed.length} failed` : ", all checked"}
          </StatusBadge>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-2 text-sm">
        {body && k.stage === "critique" ? <Critique c={body} /> : body ? <Proposal c={body} /> : null}
        {failed.length > 0 ? (
          <ul className="list-disc pl-5 text-xs text-muted-foreground">
            {failed.map((s, i) => (
              <li key={i}>
                <span className="font-mono">{s.source}</span>: {s.status}
                {s.detail ? ` (${s.detail})` : ""}
              </li>
            ))}
          </ul>
        ) : null}
        <details>
          <summary className="cursor-pointer text-xs text-muted-foreground">Raw output</summary>
          <pre className="mt-1 max-h-64 overflow-auto rounded-lg bg-muted p-2 font-mono text-xs break-words whitespace-pre-wrap">{JSON.stringify(k.content, null, 2)}</pre>
        </details>
      </CardContent>
    </Card>
  )
}
