import { useState } from "react"
import { api } from "@/api/client"
import type { ModelAssignmentSpec, ModelCheck, ModelRoleNeed, ModelsSuggestion } from "@/api/types"
import type { useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { ManagedBy } from "@/components/ManagedBy"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { humanName } from "@/lib/format"
import { PRESETS, ROLE_GROUPS, groupOf, levelText, modelOf, testsOf, type Connection } from "@/lib/models"
import { PermButton, useCan } from "@/lib/permissions"
import { TestModel } from "./test"

const TIERS = ["best", "strong", "fast"] as const

/** The model of a role in words: "claude-sonnet-4-5 on claude", or what is missing. */
function modelWords(m: ReturnType<typeof modelOf>): string {
  if (!m) return "Not set"
  return m.model ? `${m.model} on ${m.connection}` : `No ${m.tier} model on ${m.connection}`
}

/** Changes the connection, tier and model of one role, as a plan. A model not tested yet is tested first, with its cost shown. */
function ChangeRole({
  open, onOpenChange, roles, assignment, connections, checks, draft,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  roles: ModelRoleNeed[]
  assignment: ModelAssignmentSpec
  connections: Connection[]
  checks: ModelCheck[] | undefined
  draft: ReturnType<typeof useDraft>
}) {
  const usable = connections.filter((c) => c.spec.enabled)
  const [role, setRole] = useState(roles[0]?.name ?? "")
  const current = modelOf(assignment, role, connections)
  const [connection, setConnection] = useState<string>()
  const [tier, setTier] = useState<string>()
  const [model, setModel] = useState<string>()
  const conn = connection ?? current?.connection ?? usable[0]?.name ?? ""
  const tierNow = tier ?? current?.tier ?? roles.find((r) => r.name === role)?.tier ?? "strong"
  const vendor = connections.find((c) => c.name === conn)?.spec.vendor ?? ""
  const modelNow = model ?? assignment.tiers?.[tierNow]?.[vendor] ?? ""
  const tested = testsOf(checks, conn).find((t) => t.model === modelNow)
  const level = levelText(tested)
  const close = (next: boolean) => {
    onOpenChange(next)
    if (!next) {
      setConnection(undefined)
      setTier(undefined)
      setModel(undefined)
    }
  }
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const tiers = { ...assignment.tiers, [tierNow]: { ...assignment.tiers?.[tierNow], [vendor]: modelNow.trim() } }
    const spec: ModelAssignmentSpec = { ...assignment, tiers, roles: { ...assignment.roles, [role]: { connection: conn, tier: tierNow } } }
    if (await draft.make(`Use ${modelNow.trim()} for ${humanName(role)}`, [{ kind: "model_assignment", name: "organization", spec }])) close(false)
  }
  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Change a role</DialogTitle>
          <DialogDescription>Choose the model a role works with. You confirm the plan before anything changes.</DialogDescription>
        </DialogHeader>
        <form onSubmit={(e) => void submit(e)} aria-label="Change a role" className="space-y-3">
          <Field label="Role">
            {(p) => (
              <NativeSelect
                {...p}
                value={role}
                onChange={(e) => {
                  setRole(e.target.value)
                  setConnection(undefined)
                  setTier(undefined)
                  setModel(undefined)
                }}
              >
                {roles.map((r) => (
                  <NativeSelectOption key={r.name} value={r.name}>
                    {humanName(r.name)}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            )}
          </Field>
          <Field label="Provider">
            {(p) => (
              <NativeSelect {...p} value={conn} onChange={(e) => (setConnection(e.target.value), setModel(undefined))}>
                {usable.map((c) => (
                  <NativeSelectOption key={c.name} value={c.name}>
                    {c.name}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            )}
          </Field>
          <Field label="Model" hint={`Every role of the ${tierNow} tier on this provider uses it.`}>
            {(p) => <Input {...p} className="font-mono" value={modelNow} onChange={(e) => setModel(e.target.value)} />}
          </Field>
          <p className="flex flex-wrap items-center gap-2 text-sm">
            <StatusBadge tone={level.tone}>{level.text}</StatusBadge>
            {!tested && modelNow.trim() ? <TestModel connection={conn} model={modelNow.trim()} label="Test it first" /> : null}
          </p>
          {tested && tested.test?.level !== "code" ? (
            <p className="text-sm">It did not pass every test. Using it anyway asks you to confirm a second time.</p>
          ) : null}
          <details className="text-sm">
            <summary className="cursor-pointer text-muted-foreground">More options</summary>
            <div className="pt-3">
              <Field label="Tier">
                {(p) => (
                  <NativeSelect {...p} value={tierNow} onChange={(e) => (setTier(e.target.value), setModel(undefined))}>
                    {TIERS.map((t) => (
                      <NativeSelectOption key={t} value={t}>
                        {t}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                )}
              </Field>
            </div>
          </details>
          {draft.error ? (
            <p role="alert" className="text-sm text-destructive">
              {draft.error}
            </p>
          ) : null}
          <Button type="submit" disabled={draft.busy || !conn || !modelNow.trim()}>
            Review the change
          </Button>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/**
 * Who uses what: a preset makes the plan of a suggestion; each group of roles shows its model;
 * Change a role makes a plan for one role. The per-role tiers and the overrides policy are under More options.
 */
export function WhoUsesWhat({
  roles, assignment, fromFile, connections, checks, draft,
}: {
  roles: ModelRoleNeed[]
  assignment: ModelAssignmentSpec
  fromFile: boolean
  connections: Connection[]
  checks: ModelCheck[] | undefined
  draft: ReturnType<typeof useDraft>
}) {
  const can = useCan()
  const [preset, setPreset] = useState("")
  const [suggestion, setSuggestion] = useState<ModelsSuggestion>()
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const [changing, setChanging] = useState(false)
  const choose = async (p: string) => {
    setPreset(p)
    setBusy(true)
    setError("")
    try {
      const sg = await api.suggestModels(p)
      setSuggestion(sg)
      draft.setPlan(sg.plan)
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not make the suggestion")
    } finally {
      setBusy(false)
    }
  }
  const all = [...roles, ...Object.keys(assignment.roles ?? {}).filter((r) => !roles.some((x) => x.name === r)).map((name) => ({ name, tier: assignment.roles![name]!.tier, writes: false }))]
  const mayPlan = can("config.apply") && can("models.manage")
  return (
    <section aria-labelledby="who" className="space-y-3">
      <div className="flex flex-wrap items-center gap-3">
        <h2 id="who" className="text-lg font-semibold">
          Who uses what
        </h2>
        {fromFile ? <ManagedBy by="server file" reason="Until you change it here, forecrew.yaml says which model each role uses" /> : null}
        <div role="radiogroup" aria-label="Preset" className="ml-auto flex gap-1">
          {Object.entries(PRESETS).map(([value, label]) => (
            <Button
              key={value}
              role="radio"
              aria-checked={preset === value}
              variant={preset === value ? "default" : "outline"}
              size="sm"
              disabled={busy || !mayPlan}
              onClick={() => void choose(value)}
            >
              {label}
            </Button>
          ))}
        </div>
      </div>
      {!mayPlan ? (
        <p className="text-sm text-muted-foreground">
          Choosing a preset or a model needs <code className="font-mono">models.manage</code>.
        </p>
      ) : null}
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
      {suggestion ? (
        <div role="status" className="space-y-1 text-sm">
          {suggestion.plan ? null : <p>Nothing to change: the roles already use what {PRESETS[suggestion.preset] ?? suggestion.preset} would pick.</p>}
          {suggestion.notes.length ? (
            <ul className="list-disc pl-5 text-muted-foreground">
              {suggestion.notes.map((n) => (
                <li key={n}>{n}</li>
              ))}
            </ul>
          ) : null}
        </div>
      ) : null}
      <TableWrap label="Who uses what">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Does</TableHead>
              <TableHead>Roles</TableHead>
              <TableHead>Model</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {ROLE_GROUPS.map((g) => {
              const members = all.filter((r) => groupOf(r) === g)
              if (members.length === 0) return null
              const used = [...new Set(members.map((r) => modelWords(modelOf(assignment, r.name, connections))))]
              return (
                <TableRow key={g}>
                  <TableCell className="font-medium">{g}</TableCell>
                  <TableCell>{members.map((r) => humanName(r.name)).join(", ")}</TableCell>
                  <TableCell>{used.join("; ")}</TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </TableWrap>
      <PermButton permission="models.manage" variant="outline" disabled={connections.every((c) => !c.spec.enabled)} onClick={() => setChanging(true)}>
        Change a role
      </PermButton>
      <details className="text-sm">
        <summary className="cursor-pointer text-muted-foreground">More options</summary>
        <div className="space-y-3 pt-3">
          <TableWrap label="Per role">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Role</TableHead>
                  <TableHead>Tier</TableHead>
                  <TableHead>Provider</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {all.map((r) => {
                  const m = modelOf(assignment, r.name, connections)
                  return (
                    <TableRow key={r.name}>
                      <TableCell>{humanName(r.name)}</TableCell>
                      <TableCell>{m?.tier ?? r.tier}</TableCell>
                      <TableCell>{m?.connection ?? "Not set"}</TableCell>
                    </TableRow>
                  )
                })}
              </TableBody>
            </Table>
          </TableWrap>
          <p>Reviewer separation: the reviewer never uses a model of the developer's maker while a model of another maker passed its test.</p>
          <label className="flex items-start gap-2">
            <input
              type="checkbox"
              className="mt-1"
              checked={assignment.project_overrides ?? true}
              disabled={!can("models.manage") || draft.busy}
              onChange={(e) => void draft.make(e.target.checked ? "Let projects choose their own models" : "Keep every project on the organization's models", [{ kind: "model_assignment", name: "organization", spec: { ...assignment, project_overrides: e.target.checked } }])}
            />
            <span>Projects may choose their own models in their Setup.</span>
          </label>
        </div>
      </details>
      {changing ? <ChangeRole open={changing} onOpenChange={setChanging} roles={all} assignment={assignment} connections={connections} checks={checks} draft={draft} /> : null}
    </section>
  )
}
