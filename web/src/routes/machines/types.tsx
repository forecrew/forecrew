import { useState } from "react"
import { useMachines, useResources, useShapes } from "@/api/queries"
import type { ConfigResource, Shape } from "@/api/types"
import { Async } from "@/components/Async"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { ManagedBy } from "@/components/ManagedBy"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Switch } from "@/components/ui/switch"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { gigabytes, humanName } from "@/lib/format"
import { useCan } from "@/lib/permissions"
import { AddMachinesLink } from "./add-link"

/** Trust zones by the words of the interface. */
export const ZONES: Record<string, string> = { build: "Build", control: "Reasoning", actions: "GitHub Actions", device: "Devices", prod: "Production" }
const zone = (pool: string) => ZONES[pool] ?? humanName(pool)

const size = (s: Shape) => `${s.cpu} CPU · ${gigabytes(s.memory_mb)}${s.gpu ? ` · ${s.gpu} GPU` : ""}`
const price = (s: Shape) => (s.price_per_minute > 0 ? `${s.price_per_minute.toFixed(2)}/min` : "Free")

/** One machine type: what it is, and what an admin may adjust, as a plan. */
function TypeSheet({ shape, onClose }: { shape?: Shape; onClose: () => void }) {
  const can = useCan()
  const resources = useResources("shape", Boolean(shape))
  const draft = useDraft()
  const [form, setForm] = useState<{ enabled: boolean; warm: string; max: string; idle: string; pool: string; budget: string } | undefined>()
  const spec = resources.data?.find((r: ConfigResource) => r.name === shape?.name)?.spec
  const editable = Boolean(shape) && shape!.ownership === "own" && can("worker.manage")
  const f = form ?? (shape ? { enabled: shape.enabled, warm: String(shape.warm_min), max: String(shape.max), idle: String(shape.idle_seconds), pool: shape.pool, budget: shape.monthly_budget === undefined ? "" : String(shape.monthly_budget) } : undefined)
  const close = () => {
    setForm(undefined)
    void draft.discard()
    onClose()
  }
  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!shape || !f || !spec) return
    const next: Record<string, unknown> = { ...spec, enabled: f.enabled, warm_min: Number(f.warm) || 0, max: Number(f.max) || 1, idle_seconds: Number(f.idle) || 0, pool: f.pool }
    if (can("billing.manage")) {
      if (f.budget === "") delete next.monthly_budget
      else next.monthly_budget = Number(f.budget)
    }
    await draft.make(`Change machine type ${humanName(shape.name)}`, [{ kind: "shape", name: shape.name, spec: next }])
  }
  return (
    <Sheet open={Boolean(shape)} onOpenChange={(o) => !o && close()}>
      <SheetContent className="overflow-y-auto p-4">
        {shape && f ? (
          <>
            <SheetHeader className="p-0">
              <SheetTitle>{humanName(shape.name)}</SheetTitle>
              <SheetDescription>
                {size(shape)} on {humanName(shape.provider)}. A fresh machine for every job.
              </SheetDescription>
            </SheetHeader>
            {shape.ownership === "hosted" ? <ManagedBy by="Forecrew Cloud" reason="Machine types of Forecrew Cloud are read-only" /> : null}
            <dl className="space-y-1 text-sm">
              <div className="flex gap-2">
                <dt className="text-muted-foreground">Software</dt>
                <dd>{shape.image}</dd>
              </div>
              <div className="flex gap-2">
                <dt className="text-muted-foreground">Cost</dt>
                <dd>{price(shape)}</dd>
              </div>
              {shape.note ? (
                <div className="flex gap-2">
                  <dt className="text-muted-foreground">Good to know</dt>
                  <dd>{shape.note}</dd>
                </div>
              ) : null}
            </dl>
            {draft.plan ? (
              <DraftPlan draft={draft} onEdit={() => void draft.discard()} onDone={close} />
            ) : (
              <form onSubmit={(e) => void save(e)} className="space-y-3" aria-label={`Settings of ${shape.name}`}>
                <label className="flex items-center gap-2 text-sm font-medium">
                  <Switch checked={f.enabled} disabled={!editable} onCheckedChange={(v) => setForm({ ...f, enabled: v })} aria-label="On" /> On
                </label>
                <Collapsible>
                  <CollapsibleTrigger render={<Button type="button" variant="link" className="h-auto p-0" />}>More options</CollapsibleTrigger>
                  <CollapsibleContent className="mt-2 space-y-3">
                    <Field label="Ready in advance" hint="Machines kept up with no job, so the next one starts at once.">
                      {(p) => <Input {...p} inputMode="numeric" disabled={!editable} value={f.warm} onChange={(e) => setForm({ ...f, warm: e.target.value })} />}
                    </Field>
                    <Field label="Machines at once">
                      {(p) => <Input {...p} inputMode="numeric" disabled={!editable} value={f.max} onChange={(e) => setForm({ ...f, max: e.target.value })} />}
                    </Field>
                    <Field label="Keep spare for" hint="Seconds a spare machine waits for a job before it is removed.">
                      {(p) => <Input {...p} inputMode="numeric" disabled={!editable} value={f.idle} onChange={(e) => setForm({ ...f, idle: e.target.value })} />}
                    </Field>
                    <Field label="Trust zone" hint="Which agents run here, and what they may receive. Moving it to a wider zone lowers protection.">
                      {(p) => (
                        <NativeSelect {...p} disabled={!editable} value={f.pool} onChange={(e) => setForm({ ...f, pool: e.target.value })}>
                          {[...new Set(["build", "control", f.pool])].map((z) => (
                            <NativeSelectOption key={z} value={z}>
                              {zone(z)}
                            </NativeSelectOption>
                          ))}
                        </NativeSelect>
                      )}
                    </Field>
                    <Field label="Monthly limit" hint={can("billing.manage") ? "Stops new machines of this type when it is used up." : "Needs Manage budgets to change."}>
                      {(p) => <Input {...p} inputMode="decimal" disabled={!can("billing.manage") || shape.ownership !== "own"} value={f.budget} placeholder="None" onChange={(e) => setForm({ ...f, budget: e.target.value })} />}
                    </Field>
                  </CollapsibleContent>
                </Collapsible>
                {draft.error ? (
                  <p role="alert" className="text-sm text-destructive">
                    {draft.error}
                  </p>
                ) : null}
                {editable ? (
                  <Button type="submit" disabled={draft.busy || !spec}>
                    Review the change
                  </Button>
                ) : null}
              </form>
            )}
          </>
        ) : null}
      </SheetContent>
    </Sheet>
  )
}

/** Machine types: read what can run; adjust sizes when you must. Most people visit once. */
export function TypesTab() {
  const shapes = useShapes()
  const machines = useMachines()
  const can = useCan()
  const [open, setOpen] = useState<string>()
  const jobs = (name: string) => machines.data?.filter((m) => m.shape === name && m.state !== "failed").length ?? 0
  return (
    <>
      <p className="mb-3 flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
        Forecrew picks the cheapest type that fits each job.
        {can("worker.manage") ? <AddMachinesLink label="Add a machine type" /> : null}
      </p>
      <Async query={shapes} isEmpty={(d) => d.length === 0} empty={<>Machine types appear when you add a location. {can("worker.manage") ? <AddMachinesLink label="Add machines" /> : null}</>}>
        {(items) => (
          <>
            <TableWrap label="Machine types">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Type</TableHead>
                    <TableHead>Location</TableHead>
                    <TableHead>Size</TableHead>
                    <TableHead>Cost</TableHead>
                    <TableHead>Machines now</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {items.map((s) => (
                    <TableRow key={s.id}>
                      <TableCell>
                        <Button variant="link" className="h-auto p-0" onClick={() => setOpen(s.name)}>
                          {humanName(s.name)}
                        </Button>{" "}
                        {s.enabled ? null : <StatusBadge>Off</StatusBadge>}
                      </TableCell>
                      <TableCell>{humanName(s.provider)}</TableCell>
                      <TableCell>{size(s)}</TableCell>
                      <TableCell>{price(s)}</TableCell>
                      <TableCell>{jobs(s.name)}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableWrap>
            <TypeSheet shape={items.find((s) => s.name === open)} onClose={() => setOpen(undefined)} />
          </>
        )}
      </Async>
    </>
  )
}
