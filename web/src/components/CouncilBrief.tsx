import type { CouncilBrief } from "@/api/types"
import { StatusBadge } from "@/components/StatusBadge"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { levelTone, MANDATED_DISSENT } from "@/lib/format"

function List({ items, label }: { items: string[]; label: string }) {
  if (items.length === 0) return null
  return (
    <div>
      <p className="text-xs font-medium text-muted-foreground">{label}</p>
      <ul className="list-disc pl-5">
        {items.map((t, i) => (
          <li key={i}>{t}</li>
        ))}
      </ul>
    </div>
  )
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section aria-label={title} className="space-y-2">
      <h3 className="text-sm font-semibold">{title}</h3>
      {children}
    </section>
  )
}

/**
 * The decision brief of a council: options with their votes, the dissent,
 * assumptions and open questions. Everything in it was written by agents and
 * is shown as plain text. The devil's advocate's dissent is mandated by its
 * seat, so it is shown apart from the dissent of members who disagreed.
 */
export function BriefView({ brief }: { brief: CouncilBrief }) {
  const dissent = brief.dissent ?? []
  const mandated = dissent.filter((d) => d.position.startsWith(MANDATED_DISSENT))
  const real = dissent.filter((d) => !d.position.startsWith(MANDATED_DISSENT))
  const votes = brief.votes ?? []
  return (
    <div className="space-y-4 text-sm">
      <p>
        Recommendation: <strong className="font-mono">{brief.recommendation}</strong>{" "}
        {brief.reversible ? <StatusBadge>reversible</StatusBadge> : <StatusBadge tone="attention">hard to undo</StatusBadge>}
        {brief.budget_exhausted ? <StatusBadge tone="attention" className="ml-1">budget ran out</StatusBadge> : null}
      </p>
      <Section title="Options">
        <ul className="grid gap-3 md:grid-cols-2">
          {(brief.options ?? []).map((o) => {
            const forIt = votes.filter((v) => v.option === o.name)
            return (
              <li key={o.name}>
                <Card size="sm" className="h-full">
                  <CardHeader className="flex flex-row flex-wrap items-center gap-2">
                    <span className="font-mono font-medium">{o.name}</span>
                    {o.name === brief.recommendation ? <StatusBadge tone="good">recommended</StatusBadge> : null}
                    <StatusBadge tone={levelTone(o.cost)}>cost {o.cost}</StatusBadge>
                    <StatusBadge tone={levelTone(o.risk)}>risk {o.risk}</StatusBadge>
                  </CardHeader>
                  <CardContent className="space-y-2">
                    <p>{o.summary}</p>
                    <List label="Pros" items={o.pros ?? []} />
                    <List label="Cons" items={o.cons ?? []} />
                    <div>
                      <p className="text-xs font-medium text-muted-foreground">
                        Votes ({forIt.length} of {votes.length})
                      </p>
                      <ul>
                        {forIt.map((v) => (
                          <li key={v.member}>
                            <span className="font-mono">{v.member}</span>: {v.reason}
                          </li>
                        ))}
                      </ul>
                    </div>
                  </CardContent>
                </Card>
              </li>
            )
          })}
        </ul>
      </Section>
      <Section title="Dissent">
        {real.length === 0 ? (
          <p className="text-muted-foreground">No member disagreed with the recommendation.</p>
        ) : (
          <ul className="space-y-1">
            {real.map((d, i) => (
              <li key={i}>
                <span className="font-mono">{d.member}</span>: {d.position}
              </li>
            ))}
          </ul>
        )}
      </Section>
      {mandated.length > 0 ? (
        <Section title="Mandated dissent">
          <p className="text-xs text-muted-foreground">The devil&apos;s advocate must dissent; this is not a split in the council.</p>
          <ul className="space-y-1">
            {mandated.map((d, i) => (
              <li key={i}>
                <span className="font-mono">{d.member}</span>: {d.position.slice(MANDATED_DISSENT.length)}
              </li>
            ))}
          </ul>
        </Section>
      ) : null}
      <Section title="Assumptions">
        {(brief.assumptions ?? []).length === 0 ? (
          <p className="text-muted-foreground">None stated.</p>
        ) : (
          <ul className="space-y-1">
            {brief.assumptions.map((a, i) => (
              <li key={i} className="flex flex-wrap items-center gap-2">
                <span>{a.statement}</span>
                <StatusBadge tone={levelTone(a.impact)}>impact {a.impact}</StatusBadge>
                <StatusBadge>confidence {a.confidence}</StatusBadge>
                <StatusBadge>check by {a.verify_by}</StatusBadge>
              </li>
            ))}
          </ul>
        )}
      </Section>
      {(brief.open_questions ?? []).length > 0 ? (
        <Section title="Open questions">
          <ul className="list-disc pl-5">
            {brief.open_questions.map((q, i) => (
              <li key={i}>{q}</li>
            ))}
          </ul>
        </Section>
      ) : null}
    </div>
  )
}
