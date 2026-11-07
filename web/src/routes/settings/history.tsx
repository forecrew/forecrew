import { useNavigate, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { api } from "@/api/client"
import { useAudit, useMe, useMembers, usePlans, useServiceAccounts } from "@/api/queries"
import type { AuditEntry, AuditFilter, ConfigPlan } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { PlanCard } from "@/components/PlanCard"
import { StatusBadge } from "@/components/StatusBadge"
import { Field } from "@/components/Field"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { PermButton, allowed, useCan } from "@/lib/permissions"
import { sourceText, planYaml } from "@/lib/config"
import { toYaml } from "@/lib/yaml"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Section } from "./layout"

/** "member.suspended" as "Member suspended". */
export function actionText(action: string): string {
  const text = action.replaceAll(/[._]/g, " ")
  return text[0]!.toUpperCase() + text.slice(1)
}

const who = (e: AuditEntry) => e.actor_name || e.actor

/** A recorded value as text; it is data written by whoever acted, never markup. */
const show = (v: unknown) => (typeof v === "string" ? v : JSON.stringify(v))

function Details({ entry, onClose }: { entry?: AuditEntry; onClose: () => void }) {
  return (
    <Sheet open={Boolean(entry)} onOpenChange={(open) => !open && onClose()}>
      <SheetContent className="overflow-y-auto p-4">
        {entry ? (
          <>
            <SheetHeader className="p-0">
              <SheetTitle>{actionText(entry.action)}</SheetTitle>
              <SheetDescription>
                {who(entry)} · <time dateTime={entry.created_at}>{new Date(entry.created_at).toLocaleString()}</time>
              </SheetDescription>
            </SheetHeader>
            <dl className="mt-4 space-y-2 text-sm">
              <div>
                <dt className="text-muted-foreground">Actor</dt>
                <dd className="font-mono break-all">{entry.actor}</dd>
              </div>
              {entry.credential ? (
                <div>
                  <dt className="text-muted-foreground">Through</dt>
                  <dd className="font-mono break-all">{entry.credential}</dd>
                </div>
              ) : null}
              <div>
                <dt className="text-muted-foreground">What it was done to</dt>
                <dd className="font-mono break-all">
                  {entry.subject_type} {entry.subject_id}
                </dd>
              </div>
              {Object.entries(entry.data ?? {}).map(([k, v]) => (
                <div key={k}>
                  <dt className="text-muted-foreground">{k}</dt>
                  <dd className="break-words whitespace-pre-wrap">{show(v)}</dd>
                </div>
              ))}
            </dl>
          </>
        ) : null}
      </SheetContent>
    </Sheet>
  )
}

/** All activity: the audit log, who did what and when, with filters. */
function ActivityTab() {
  const [actor, setActor] = useState("")
  const [action, setAction] = useState("")
  const [type, setType] = useState("")
  const [since, setSince] = useState("")
  const [until, setUntil] = useState("")
  const [open, setOpen] = useState<AuditEntry>()
  const members = useMembers()
  const accounts = useServiceAccounts()
  const filter: AuditFilter = {
    actor, action: action.trim(), subject_type: type.trim(),
    since: since ? `${since}T00:00:00Z` : "", until: until ? `${until}T23:59:59Z` : "",
  }
  const entries = useAudit(filter)
  const clear = () => {
    setActor("")
    setAction("")
    setType("")
    setSince("")
    setUntil("")
  }

  return (
    <>
      <form className="mb-4 flex flex-wrap items-end gap-3" aria-label="Filters" onSubmit={(e) => e.preventDefault()}>
        <Field label="Who">
          {(p) => (
            <NativeSelect {...p} value={actor} onChange={(e) => setActor(e.target.value)}>
              <NativeSelectOption value="">Anyone</NativeSelectOption>
              {(members.data ?? []).map((m) => (
                <NativeSelectOption key={m.user_id} value={`user:${m.user_id}`}>
                  {m.name}
                </NativeSelectOption>
              ))}
              {(accounts.data ?? []).map((a) => (
                <NativeSelectOption key={a.id} value={`service_account:${a.id}`}>
                  {a.name} (service account)
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </Field>
        <Field label="Action" hint="For example member.removed">
          {(p) => <Input {...p} className="w-44 font-mono" value={action} onChange={(e) => setAction(e.target.value)} />}
        </Field>
        <Field label="Done to" hint="For example team">
          {(p) => <Input {...p} className="w-32 font-mono" value={type} onChange={(e) => setType(e.target.value)} />}
        </Field>
        <Field label="From">{(p) => <Input {...p} type="date" value={since} onChange={(e) => setSince(e.target.value)} />}</Field>
        <Field label="To">{(p) => <Input {...p} type="date" value={until} onChange={(e) => setUntil(e.target.value)} />}</Field>
        <Button type="button" variant="outline" onClick={clear}>
          Clear filters
        </Button>
      </form>
      <Async query={entries} isEmpty={(d) => d.length === 0} empty="No changes yet. Everything changed here, by anyone or anything, will be listed.">
        {(items) => (
          <TableWrap label="Change history">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>What</TableHead>
                  <TableHead>Who</TableHead>
                  <TableHead>Done to</TableHead>
                  <TableHead>Details</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((e) => (
                  <TableRow key={e.id}>
                    <TableCell>
                      <Age iso={e.created_at} ago />
                    </TableCell>
                    <TableCell>{actionText(e.action)}</TableCell>
                    <TableCell>{who(e)}</TableCell>
                    <TableCell>
                      <span className="font-mono text-sm">{e.subject_type}</span>
                    </TableCell>
                    <TableCell>
                      <Button variant="ghost" aria-label={`Details of ${actionText(e.action)} ${e.id}`} onClick={() => setOpen(e)}>
                        Details
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        )}
      </Async>
      <Details entry={open} onClose={() => setOpen(undefined)} />
    </>
  )
}

/** Who made a plan: a person by name, otherwise what the actor says. */
function useWho() {
  const members = useMembers()
  return (actor: string) => {
    const id = actor.replace(/^user:/, "")
    return members.data?.find((m) => m.user_id === id)?.name ?? actor.replace(/^[a-z_]+:/, "")
  }
}

function dayOf(iso: string, now = Date.now()): string {
  const d = new Date(iso)
  const days = Math.floor((new Date(now).setHours(0, 0, 0, 0) - new Date(d).setHours(0, 0, 0, 0)) / 86_400_000)
  return days <= 0 ? "Today" : days === 1 ? "Yesterday" : d.toLocaleDateString()
}

function statusOf(p: ConfigPlan): { text: string; tone?: "good" | "attention" | "bad" } {
  switch (p.status) {
    case "applied":
      return p.pr_url ? { text: "PR open", tone: "attention" } : { text: "Applied", tone: "good" }
    case "reverted":
      return { text: "Reverted" }
    case "awaiting_approval":
      return { text: "Waiting for approval", tone: "attention" }
    case "pending":
      return { text: "Not applied", tone: "attention" }
    case "canceled":
      return { text: "Canceled" }
    default:
      return { text: "Expired" }
  }
}

/** A plan as it was confirmed: the diff, who asked and who confirmed, and the YAML before and after. Revert makes a new plan. */
function PlanDetails({ plan, who, onClose }: { plan?: ConfigPlan; who: (a: string) => string; onClose: () => void }) {
  const [revert, setRevert] = useState<ConfigPlan>()
  const [error, setError] = useState("")
  const can = useMe().data
  const close = () => {
    setRevert(undefined)
    setError("")
    onClose()
  }
  const doRevert = async () => {
    if (!plan) return
    setError("")
    try {
      setRevert(await api.revertPlan(plan.id))
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not make a plan to revert it")
    }
  }
  const before = plan?.changes.map((c) => (c.old ? toYaml({ kind: c.kind, name: c.name, spec: c.old }) : `# new: ${c.kind}/${c.name}\n`)).join("---\n")
  return (
    <Sheet open={Boolean(plan)} onOpenChange={(o) => !o && close()}>
      <SheetContent className="overflow-y-auto p-4 sm:max-w-xl">
        {plan ? (
          <>
            <SheetHeader className="p-0">
              <SheetTitle>{plan.title}</SheetTitle>
              <SheetDescription>
                {who(plan.created_by)} · {sourceText(plan)} · <time dateTime={plan.created_at}>{new Date(plan.created_at).toLocaleString()}</time>
              </SheetDescription>
            </SheetHeader>
            <dl className="space-y-2 text-sm">
              {plan.request ? (
                <div>
                  <dt className="text-muted-foreground">Asked</dt>
                  <dd className="break-words">{plan.request}</dd>
                </div>
              ) : null}
              <div>
                <dt className="text-muted-foreground">Confirmed by</dt>
                <dd>{plan.applied_by ? who(plan.applied_by) : "Nobody"}</dd>
              </div>
              <div>
                <dt className="text-muted-foreground">Affected</dt>
                <dd>{plan.changes.map((c) => `${c.kind}/${c.name}`).join(", ") || "Nothing"}</dd>
              </div>
              <div>
                <dt className="text-muted-foreground">Undo</dt>
                <dd>{plan.undo || (plan.reversible ? "Can be reverted." : "Cannot be reverted.")}</dd>
              </div>
            </dl>
            <ul className="space-y-1 rounded-lg bg-muted p-2.5 font-mono text-xs">
              {plan.changes.map((c) => (
                <li key={`${c.kind}/${c.name}`} className="break-words whitespace-pre-wrap">
                  {c.line}
                </li>
              ))}
            </ul>
            <div className="grid gap-2 sm:grid-cols-2">
              <div>
                <h3 className="text-sm font-medium">YAML before</h3>
                <pre tabIndex={0} className="max-h-60 overflow-auto rounded-lg bg-muted p-2 font-mono text-xs">
                  {before}
                </pre>
              </div>
              <div>
                <h3 className="text-sm font-medium">YAML after</h3>
                <pre tabIndex={0} className="max-h-60 overflow-auto rounded-lg bg-muted p-2 font-mono text-xs">
                  {planYaml(plan)}
                </pre>
              </div>
            </div>
            {plan.status === "applied" && allowed(can, "config.apply") && plan.needs.every((n) => allowed(can, n)) && !revert ? (
              <Button variant="outline" onClick={() => void doRevert()}>
                Revert
              </Button>
            ) : null}
            {error ? (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            ) : null}
            {revert ? <PlanCard plan={revert} onChange={setRevert} /> : null}
          </>
        ) : null}
      </SheetContent>
    </Sheet>
  )
}

/** Everything as YAML, except secrets, and a place to paste YAML that becomes a plan like any other. */
function ExportImport() {
  const can = useCan()
  const [yaml, setYaml] = useState("")
  const [imported, setImported] = useState<ConfigPlan>()
  const [text, setText] = useState("")
  const [error, setError] = useState("")
  return (
    <div className="flex gap-2">
      {can("config.read") ? (
        <Dialog onOpenChange={(o) => o && api.exportConfig().then((r) => setYaml(r.yaml), (e: Error) => setYaml(`# ${e.message}`))}>
          <DialogTrigger render={<Button variant="outline" />}>Export YAML</DialogTrigger>
          <DialogContent className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>Export YAML</DialogTitle>
              <DialogDescription>Everything except secrets, which are only ever references.</DialogDescription>
            </DialogHeader>
            <pre tabIndex={0} className="max-h-96 overflow-auto rounded-lg bg-muted p-3 font-mono text-xs">
              {yaml || "Loading…"}
            </pre>
          </DialogContent>
        </Dialog>
      ) : (
        <PermButton permission="config.read" variant="outline">
          Export YAML
        </PermButton>
      )}
      {can("config.apply") ? (
        <Dialog onOpenChange={(o) => !o && (setImported(undefined), setError(""), setText(""))}>
          <DialogTrigger render={<Button variant="outline" />}>Import</DialogTrigger>
          <DialogContent className="max-w-2xl">
            <DialogHeader>
              <DialogTitle>Import YAML</DialogTitle>
              <DialogDescription>It makes a plan to confirm, like any other change.</DialogDescription>
            </DialogHeader>
            {imported ? (
              <PlanCard plan={imported} onChange={setImported} />
            ) : (
              <form
                className="space-y-2"
                onSubmit={(e) => {
                  e.preventDefault()
                  setError("")
                  api.importConfig(text).then(setImported, (err: Error) => setError(err.message))
                }}
              >
                <Field label="YAML">{(p) => <Textarea {...p} rows={8} className="font-mono text-xs" value={text} onChange={(e) => setText(e.target.value)} />}</Field>
                {error ? (
                  <p role="alert" className="text-sm text-destructive">
                    {error}
                  </p>
                ) : null}
                <Button type="submit" disabled={!text.trim()}>
                  Make a plan
                </Button>
              </form>
            )}
          </DialogContent>
        </Dialog>
      ) : (
        <PermButton permission="config.apply" variant="outline">
          Import
        </PermButton>
      )}
    </div>
  )
}

/** Settings changes: each plan, who did it, what it did, and how to undo it. */
function ChangesTab() {
  const plans = usePlans(false)
  const who = useWho()
  const [open, setOpen] = useState<string>()
  return (
    <Async query={plans} isEmpty={(d) => d.length === 0} empty="No changes yet. Everything changed here, by anyone or anything, will be listed.">
      {(items) => {
        const sorted = [...items].sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at))
        const days = [...new Set(sorted.map((p) => dayOf(p.created_at)))]
        return (
          <>
            {days.map((day) => (
              <section key={day} aria-label={day} className="mb-4">
                <h2 className="mb-1 text-sm font-medium text-muted-foreground">{day}</h2>
                <ul className="divide-y rounded-xl border bg-card">
                  {sorted
                    .filter((p) => dayOf(p.created_at) === day)
                    .map((p) => {
                      const st = statusOf(p)
                      return (
                        <li key={p.id} className="flex flex-wrap items-center gap-x-4 gap-y-1 px-3 py-2 text-sm">
                          <time dateTime={p.created_at} className="w-12 text-muted-foreground">
                            {new Date(p.created_at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
                          </time>
                          <Button variant="link" className="h-auto min-w-0 flex-1 justify-start p-0 text-left" onClick={() => setOpen(p.id)}>
                            {p.title}
                          </Button>
                          <span className="text-muted-foreground">
                            {who(p.created_by)} · {sourceText(p)}
                          </span>
                          <StatusBadge tone={st.tone}>{st.text}</StatusBadge>
                          <span className="w-16 text-right text-muted-foreground">{p.status === "applied" ? (p.reversible ? "Can revert" : "Can’t") : ""}</span>
                        </li>
                      )
                    })}
                </ul>
              </section>
            ))}
            <PlanDetails plan={items.find((p) => p.id === open)} who={who} onClose={() => setOpen(undefined)} />
          </>
        )
      }}
    </Async>
  )
}

/** Change history: settings changes and the full audit log, in one place, because two logs make people ask which to believe. */
export function HistoryPage() {
  const me = useMe().data
  const navigate = useNavigate()
  const { tab } = useSearch({ strict: false }) as { tab?: string }
  const audit = allowed(me, "audit.read")
  const current = tab === "activity" && audit ? "activity" : "changes"
  return (
    <Section permission="audit.read" or="config.apply" title="Change history" meta="Everything changed here, by anyone or anything, with who and when." actions={<ExportImport />}>
      <Tabs value={current} onValueChange={(v) => void navigate({ to: "/settings/history", search: (v === "activity" ? { tab: "activity" } : {}) as never })}>
        <TabsList aria-label="Change history">
          <TabsTrigger value="changes">Settings changes</TabsTrigger>
          {audit ? <TabsTrigger value="activity">All activity</TabsTrigger> : null}
        </TabsList>
      </Tabs>
      <div className="mt-4">{current === "activity" ? <ActivityTab /> : <ChangesTab />}</div>
    </Section>
  )
}
