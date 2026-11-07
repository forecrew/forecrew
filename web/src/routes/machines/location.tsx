import { Link, useParams } from "@tanstack/react-router"
import { useState } from "react"
import { useProviders, useResources, useShapes } from "@/api/queries"
import type { Provider } from "@/api/types"
import { Age } from "@/components/Age"
import { AskButton } from "@/components/assistant/AskButton"
import { Async } from "@/components/Async"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { ManagedBy } from "@/components/ManagedBy"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Input } from "@/components/ui/input"
import { gigabytes, humanName } from "@/lib/format"
import { toYaml } from "@/lib/yaml"
import { useCan } from "@/lib/permissions"
import { YamlDialog } from "@/components/YamlDialog"
import { RegisterImage } from "./register-image"

const CHECK_NAMES: Record<string, string> = { kvm: "KVM", disk: "Disk", software: "Software", sleep: "Sleep", hyperv: "Hyper-V" }
const KIND_TEXT: Record<string, string> = { microvm: "Linux", proxmox: "Proxmox", hyperv: "Windows", tart: "macOS", aws: "AWS", fake: "Test" }

function Facts({ p }: { p: Provider }) {
  const c = p.capabilities
  const bits = [
    KIND_TEXT[p.kind] ?? p.kind,
    c.max_cpu ? `${c.max_cpu} CPU` : "",
    c.max_memory_mb ? gigabytes(c.max_memory_mb) : "",
    c.region ?? "",
    p.last_seen_at ? "" : "never connected",
    c.agent_version ? `version ${c.agent_version}` : "",
  ].filter(Boolean)
  return <p className="text-sm text-muted-foreground">{bits.join(" · ")}</p>
}

/** One location: is it healthy, what can it do, how much may it use. */
export function LocationPage() {
  const { id } = useParams({ strict: false }) as { id: string }
  const providers = useProviders()
  const shapes = useShapes()
  const resources = useResources("provider")
  const can = useCan()
  const draft = useDraft()
  const [limit, setLimit] = useState<string>()
  const [budget, setBudget] = useState<string>()
  const manage = can("worker.manage")

  return (
    <Async query={providers}>
      {(all) => {
        const p = all.find((x) => x.id === id)
        if (!p) {
          return (
            <>
              <PageHeader title="Location not found" />
              <Link to="/machines" className="underline underline-offset-2">
                ← Machines
              </Link>
            </>
          )
        }
        const res = resources.data?.find((r) => r.name === p.name)
        const spec = (res?.spec ?? {}) as { limits?: { max_machines?: number; monthly_budget?: number }; paused?: boolean }
        const fromFile = res?.origin === "file"
        const mine = p.ownership === "own" && !fromFile
        const types = (shapes.data ?? []).filter((s) => s.provider === p.name)
        const maxNow = limit ?? String(p.limits.max_machines ?? "")
        const budgetNow = budget ?? (p.limits.monthly_budget === undefined ? "" : String(p.limits.monthly_budget))
        const change = (next: Record<string, unknown>, title: string) => draft.make(title, [{ kind: "provider", name: p.name, spec: { ...spec, ...next } }])
        const submitLimits = async (e: React.FormEvent) => {
          e.preventDefault()
          const limits = { ...spec.limits }
          if (manage) limits.max_machines = Number(maxNow) || undefined
          if (can("billing.manage")) limits.monthly_budget = budgetNow === "" ? undefined : Number(budgetNow)
          await change({ limits }, `Change limits of ${humanName(p.name)}`)
        }
        return (
          <>
            <Link to="/machines" className="mb-2 inline-block text-sm underline underline-offset-2">
              ← Machines
            </Link>
            <PageHeader title={humanName(p.name)} badge={<StatusBadge tone={p.online ? "good" : p.status === "paused" ? "attention" : "bad"}>{p.status === "paused" ? "Paused" : p.online ? "Online" : p.status === "pending" ? "Waiting to connect" : "Offline"}</StatusBadge>} meta={<Facts p={p} />}>
              <AskButton message={`Tell me about ${humanName(p.name)}`} />
              {mine && manage ? (
                <Button variant="outline" disabled={draft.busy} onClick={() => void change({ paused: p.status !== "paused" }, `${p.status === "paused" ? "Resume" : "Pause"} ${humanName(p.name)}`)}>
                  {p.status === "paused" ? "Resume" : "Pause"}
                </Button>
              ) : null}
              <YamlDialog title={`${humanName(p.name)} as YAML`} yaml={toYaml({ kind: "provider", name: p.name, spec })} label="⋯ View as YAML" />
            </PageHeader>
            {fromFile ? <ManagedBy by="server file" reason="Set in forecrew.yaml; change it there" /> : null}
            {!p.online && p.status !== "paused" ? (
              <Alert className="mb-4">
                <AlertTitle>This location is offline</AlertTitle>
                <AlertDescription>
                  Last seen {p.last_seen_at ? <Age iso={p.last_seen_at} ago /> : "never"}. Check that the machine is on and awake, and that the Forecrew connector is running. Jobs that need it wait until it is back.
                </AlertDescription>
              </Alert>
            ) : null}
            {draft.plan ? (
              <div className="mb-4 max-w-xl">
                <DraftPlan draft={draft} onEdit={() => void draft.discard()} onDone={() => void draft.discard()} />
              </div>
            ) : null}
            <div className="grid gap-6 md:grid-cols-2">
              <section aria-labelledby="checks" className="space-y-2">
                <h2 id="checks" className="text-lg font-semibold">
                  Checks
                </h2>
                {(p.capabilities.checks ?? []).length === 0 ? <p className="text-sm text-muted-foreground">Nothing reported yet.</p> : null}
                <ul className="space-y-1 text-sm">
                  {(p.capabilities.checks ?? []).map((c) => (
                    <li key={c.name} className="flex justify-between gap-3">
                      <span>{CHECK_NAMES[c.name] ?? humanName(c.name)}</span>
                      <span>
                        <StatusBadge tone={c.status === "ok" ? "good" : c.status === "warn" ? "attention" : "bad"}>{c.status === "ok" ? "OK" : c.status === "warn" ? "Check this" : "Failed"}</StatusBadge> {c.detail}
                      </span>
                    </li>
                  ))}
                </ul>
              </section>
              <section aria-labelledby="limits" className="space-y-2">
                <h2 id="limits" className="text-lg font-semibold">
                  Limits
                </h2>
                <form onSubmit={(e) => void submitLimits(e)} className="space-y-3" aria-label="Limits">
                  <Field label="Machines at once">{(f) => <Input {...f} inputMode="numeric" disabled={!manage || !mine} value={maxNow} onChange={(e) => setLimit(e.target.value)} />}</Field>
                  <Field label="Monthly limit" hint={can("billing.manage") ? "Jobs wait when it is used up." : p.limits.monthly_budget === undefined ? "Not needed. Changing it needs Manage budgets." : "Changing it needs Manage budgets."}>
                    {(f) => <Input {...f} inputMode="decimal" placeholder="Not needed" disabled={!can("billing.manage") || !mine} value={budgetNow} onChange={(e) => setBudget(e.target.value)} />}
                  </Field>
                  {mine && (manage || can("billing.manage")) ? (
                    <Button type="submit" disabled={draft.busy}>
                      Review the change
                    </Button>
                  ) : null}
                  {draft.error ? (
                    <p role="alert" className="text-sm text-destructive">
                      {draft.error}
                    </p>
                  ) : null}
                </form>
              </section>
            </div>
            <section aria-labelledby="types" className="mt-6 space-y-2">
              <h2 id="types" className="text-lg font-semibold">
                Machine types here
              </h2>
              {types.length === 0 ? <p className="text-sm text-muted-foreground">None yet.</p> : null}
              <ul className="text-sm">
                {types.map((s) => (
                  <li key={s.id}>
                    {humanName(s.name)} <span className="text-muted-foreground">{s.cpu} CPU · {gigabytes(s.memory_mb)}</span>
                  </li>
                ))}
              </ul>
            </section>
            <Collapsible className="mt-6">
              <CollapsibleTrigger render={<Button variant="link" className="h-auto p-0" />}>More options</CollapsibleTrigger>
              <CollapsibleContent className="mt-2 space-y-2 text-sm">
                <p>
                  Network allowlist: {p.network_allow.length ? <span className="font-mono">{p.network_allow.join(", ")}</span> : "internet only"}
                </p>
                <p>
                  Tags: {Object.keys(p.tags).length ? <span className="font-mono">{Object.entries(p.tags).map(([k, v]) => `${k}=${v}`).join(", ")}</span> : "none"}
                </p>
                {manage && (p.capabilities.os ?? []).includes("windows") ? <RegisterImage /> : null}
              </CollapsibleContent>
            </Collapsible>
            {mine && manage ? (
              <p className="mt-6">
                <Button
                  variant="link"
                  className="h-auto p-0 text-destructive"
                  disabled={draft.busy}
                  onClick={() => void draft.make(`Remove ${humanName(p.name)}`, [{ kind: "provider", name: p.name, delete: true }])}
                >
                  Remove location…
                </Button>
              </p>
            ) : null}
          </>
        )
      }}
    </Async>
  )
}
