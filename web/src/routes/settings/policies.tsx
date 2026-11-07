import { useState } from "react"
import { useResources, useShapes } from "@/api/queries"
import type { ConfigResource, FleetRouting } from "@/api/types"
import { Async } from "@/components/Async"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Textarea } from "@/components/ui/textarea"
import { humanName } from "@/lib/format"
import { useCan } from "@/lib/permissions"
import { ZONES } from "@/routes/machines/types"
import { Section } from "./layout"

const ROUTING: { value: FleetRouting; label: string }[] = [
  { value: "own_only", label: "only my machines" },
  { value: "own_first", label: "my machines first, then Forecrew Cloud" },
  { value: "hosted", label: "Forecrew Cloud" },
]

interface Policy {
  routing: FleetRouting
  quotas?: { concurrent_machines?: number; monthly_budget?: number; monthly_minutes?: number }
  egress?: string[]
  [k: string]: unknown
}

/** A limit as text; an empty field is no limit. */
const num = (v: string): number | undefined => (v.trim() === "" || Number.isNaN(Number(v)) ? undefined : Number(v))

function Rules({ resource }: { resource: ConfigResource }) {
  const policy = resource.spec as Policy
  const can = useCan()
  const draft = useDraft()
  const seesZones = can("worker.read")
  const shapes = useShapes(seesZones)
  const [routing, setRouting] = useState<FleetRouting>()
  const [machines, setMachines] = useState<string>()
  const [budget, setBudget] = useState<string>()
  const [egress, setEgress] = useState<string>()
  const manage = can("worker.manage")
  const money = can("billing.manage")
  const routeNow = routing ?? policy.routing
  const machinesNow = machines ?? String(policy.quotas?.concurrent_machines ?? "")
  const budgetNow = budget ?? String(policy.quotas?.monthly_budget ?? "")
  const egressNow = egress ?? (policy.egress ?? []).join("\n")
  const fromFile = resource.origin === "file"

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const next: Policy = { ...policy, routing: manage ? routeNow : policy.routing }
    const quotas = { ...policy.quotas }
    if (money) {
      quotas.concurrent_machines = num(machinesNow)
      quotas.monthly_budget = num(budgetNow)
    }
    next.quotas = Object.fromEntries(Object.entries(quotas).filter(([, v]) => v !== undefined))
    if (manage) next.egress = egressNow.split(/[\s,]+/).filter(Boolean)
    await draft.make("Change where jobs run", [{ kind: "fleet_policy", name: "fleet", spec: next }])
  }
  const zones = [...new Set((shapes.data ?? []).map((s) => s.pool))]

  return (
    <div className="max-w-2xl space-y-6">
      {draft.plan ? (
        <DraftPlan draft={draft} onEdit={() => void draft.discard()} onDone={() => void draft.discard()} />
      ) : (
        <form onSubmit={(e) => void submit(e)} className="space-y-6" aria-label="Policies">
          <section aria-labelledby="where" className="space-y-3">
            <h2 id="where" className="text-lg font-semibold">
              Where jobs run
            </h2>
            <Field label="Jobs run on" hint={manage ? "Once you connect your own machines, nothing should leave them without you asking." : "Needs Manage machines to change."}>
              {(p) => (
                <NativeSelect {...p} disabled={!manage || fromFile} value={routeNow} onChange={(e) => setRouting(e.target.value as FleetRouting)}>
                  {ROUTING.map((r) => (
                    <NativeSelectOption key={r.value} value={r.value}>
                      {r.label}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              )}
            </Field>
            <Field label="Run at most (machines at once)" hint={money ? undefined : "Needs Manage budgets to change."}>
              {(p) => <Input {...p} inputMode="numeric" disabled={!money || fromFile} placeholder="No limit" value={machinesNow} onChange={(e) => setMachines(e.target.value)} />}
            </Field>
            <Field label="Stop at (a month)" hint={money ? "Jobs wait when it is used up, and you get an Inbox notice at 80%." : "Needs Manage budgets to change."}>
              {(p) => <Input {...p} inputMode="decimal" disabled={!money || fromFile} placeholder="No limit" value={budgetNow} onChange={(e) => setBudget(e.target.value)} />}
            </Field>
          </section>
          <section aria-labelledby="network" className="space-y-3">
            <h2 id="network" className="text-lg font-semibold">
              Network
            </h2>
            <Field label="Machines can also reach" hint="One host or address range per line, on top of model APIs, GitHub and package registries. Opening network access lowers protection.">
              {(p) => <Textarea {...p} rows={3} className="font-mono text-xs" disabled={!manage || fromFile} value={egressNow} onChange={(e) => setEgress(e.target.value)} />}
            </Field>
          </section>
          {draft.error ? (
            <p role="alert" className="text-sm text-destructive">
              {draft.error}
            </p>
          ) : null}
          {(manage || money) && !fromFile ? (
            <Button type="submit" disabled={draft.busy}>
              Review the change
            </Button>
          ) : null}
          {fromFile ? <p className="text-sm text-muted-foreground">Managed by server file: change it in forecrew.yaml.</p> : null}
        </form>
      )}
      <Collapsible>
        <CollapsibleTrigger render={<Button variant="link" className="h-auto p-0" />}>Trust zones</CollapsibleTrigger>
        <CollapsibleContent className="mt-2 space-y-2 text-sm">
          <p className="text-muted-foreground">Which agents run where, and what they may receive. Each machine type has one; change it from its settings on Machines.</p>
          {seesZones ? (
            <ul>
              {zones.map((z) => (
                <li key={z}>
                  <span className="font-medium">{ZONES[z] ?? humanName(z)}</span>:{" "}
                  {(shapes.data ?? []).filter((s) => s.pool === z).map((s) => humanName(s.name)).join(", ")}
                </li>
              ))}
            </ul>
          ) : (
            <p>
              Hidden. Seeing which machine types run in each zone needs <code className="font-mono">worker.read</code>.
            </p>
          )}
        </CollapsibleContent>
      </Collapsible>
    </div>
  )
}

/** Policies: rules written as sentences, with one control each. */
export function PoliciesPage() {
  const policy = useResources("fleet_policy")
  return (
    <Section permission="worker.read" or="billing.manage" title="Policies" meta="Rules about where jobs run and what they may use.">
      <Async query={policy} isEmpty={(d) => d.length === 0} empty="No policy yet.">
        {(rows) => <Rules resource={rows[0]!} />}
      </Async>
    </Section>
  )
}
