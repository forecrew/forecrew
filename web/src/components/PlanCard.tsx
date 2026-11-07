import { useQueryClient } from "@tanstack/react-query"
import { Link } from "@tanstack/react-router"
import { useState } from "react"
import { api } from "@/api/client"
import { CONFIG_KEYS } from "@/api/queries"
import type { ConfigApplied, ConfigChange, ConfigPlan, ShowOnceValue } from "@/api/types"
import { SecureField } from "@/components/SecureField"
import { ShowOnce } from "@/components/ShowOnce"
import { StatusBadge } from "@/components/StatusBadge"
import { YamlDialog } from "@/components/YamlDialog"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardFooter, CardHeader } from "@/components/ui/card"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { appliesText, narrowScreen, permissionWords, planYaml, sourceText } from "@/lib/config"
import { age, until } from "@/lib/format"

/** The sign of a change, which carries the meaning (the color does not). */
const SIGN = { create: "+", update: "~", delete: "−" } as const

function ChangeLine({ c }: { c: ConfigChange }) {
  return (
    <li className="space-y-0.5">
      <p className="font-mono text-xs break-words whitespace-pre-wrap">
        <span aria-hidden="true">{SIGN[c.op]} </span>
        <span className="sr-only">{{ create: "Add", update: "Change", delete: "Remove" }[c.op]}: </span>
        {c.line.replace(/^[+~−-]\s*/, "")}
      </p>
      {c.fields.length > 0 && c.op === "update" ? (
        <ul className="ml-4 space-y-0.5 font-mono text-xs text-muted-foreground">
          {c.fields.map((f) => (
            <li key={f.path} className="break-words">
              {f.path}: {f.old || "none"} → {f.new || "none"}
            </li>
          ))}
        </ul>
      ) : null}
    </li>
  )
}

/** The second confirmation of a plan that lowers protection: the same person types the word shown. */
function SecondConfirmation({ plan, open, busy, onClose, onConfirm }: { plan: ConfigPlan; open: boolean; busy: boolean; onClose: () => void; onConfirm: (word: string) => void }) {
  const [word, setWord] = useState("")
  const word0 = plan.confirm_word ?? ""
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Confirm: {plan.title}</DialogTitle>
          <DialogDescription>This lowers protection. One more step, to be sure it is you and it is meant.</DialogDescription>
        </DialogHeader>
        <ul className="list-disc space-y-1 pl-5 text-sm font-medium text-destructive">
          {plan.lowers_protection.map((r) => (
            <li key={r}>{r}</li>
          ))}
        </ul>
        <label className="block space-y-1.5 text-sm">
          <span>
            Type <code className="font-mono font-semibold">{word0}</code> to confirm
          </span>
          <Input value={word} autoComplete="off" spellCheck={false} className="font-mono" onChange={(e) => setWord(e.target.value)} />
        </label>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            Keep it as it is
          </Button>
          <Button variant="destructive" disabled={busy || word !== word0} onClick={() => onConfirm(word)}>
            Confirm and apply
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

interface Props {
  plan: ConfigPlan
  /** Called with the plan after each step (a secret saved, applied, canceled). */
  onChange?: (plan: ConfigPlan) => void
  /** Opens the form the page uses, filled in; the card offers Edit when this is set. */
  onEdit?: () => void
  /** What the button that drops the plan says; Reject in the Inbox. */
  cancelLabel?: string
  /** Replaces a stale plan with a new one made from the same changes. */
  onRebuilt?: (plan: ConfigPlan) => void
}

/**
 * A change plan, drawn from server data only: what changes, what it affects, what it costs,
 * whether it can be undone, and the buttons that confirm it. A sentence never confirms a plan:
 * a button is tied to this stored plan.
 */
export function PlanCard({ plan: given, onChange, onEdit, onRebuilt, cancelLabel = "Cancel" }: Props) {
  const qc = useQueryClient()
  const [override, setOverride] = useState<{ base: string; plan: ConfigPlan }>()
  const [showOnce, setShowOnce] = useState<ShowOnceValue[]>([])
  const [second, setSecond] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const [asked, setAsked] = useState("")
  // A plan the page read again beats what this card last did.
  const plan = override && override.base === `${given.id}${given.updated_at}${given.status}` ? override.plan : given
  const refresh = () => Promise.all(CONFIG_KEYS.map((queryKey) => qc.invalidateQueries({ queryKey })))
  const set = (next: ConfigPlan) => {
    setOverride({ base: `${given.id}${given.updated_at}${given.status}`, plan: next })
    onChange?.(next)
  }
  const run = async (fn: () => Promise<void>) => {
    setBusy(true)
    setError("")
    try {
      await fn()
      await refresh()
    } catch (err) {
      setError(err instanceof Error ? err.message : "Nothing changed")
    } finally {
      setBusy(false)
    }
  }
  const apply = (word?: string) =>
    run(async () => {
      const done: ConfigApplied = await api.applyPlan(plan.id, word)
      setSecond(false)
      setShowOnce(done.show_once ?? [])
      set(done.plan)
    })
  const open = plan.status === "pending" || plan.status === "awaiting_approval"
  const needsSecret = plan.steps.some((s) => s.kind === "secret_required" && !s.filled)
  const lowers = plan.lowers_protection.length > 0
  const phone = lowers && narrowScreen()

  return (
    <Card role="region" aria-label={`Change plan: ${plan.title}`} size="sm">
      <CardHeader>
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p className="text-xs font-medium tracking-wide text-muted-foreground uppercase">Change plan</p>
          <StatusBadge>{appliesText(plan.applies)}</StatusBadge>
        </div>
        <h3 className="text-base font-medium">{plan.title}</h3>
        {plan.request ? <p className="text-sm text-muted-foreground">You asked: {plan.request}</p> : null}
        {plan.source === "mcp" ? <p className="text-sm text-muted-foreground">Plan from {sourceText(plan)}</p> : null}
        {lowers ? (
          <ul className="space-y-1" aria-label="Lowers protection">
            {plan.lowers_protection.map((r) => (
              <li key={r}>
                <StatusBadge tone="bad">Lowers protection</StatusBadge> <span className="text-sm">{r}</span>
              </li>
            ))}
          </ul>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-3 text-sm">
        {plan.changes.length > 0 ? (
          <section aria-label="Changes" className="space-y-1">
            <h4 className="font-medium">Changes</h4>
            <ul className="space-y-1.5 rounded-lg bg-muted p-2.5">
              {plan.changes.map((c) => (
                <ChangeLine key={`${c.kind}/${c.name}`} c={c} />
              ))}
            </ul>
          </section>
        ) : null}
        {plan.steps.map((s) =>
          s.kind === "secret_required" ? (
            <SecureField key={s.secret ?? s.title} label={s.title} filled={s.filled} reuses={s.reuses} onSave={async (v) => {
                set(await api.putPlanSecret(plan.id, s.secret ?? "", v))
                await refresh()
              }} />
          ) : (
            <div key={s.title} className="rounded-lg border p-3">
              <p className="text-xs font-medium tracking-wide text-muted-foreground uppercase">Connect</p>
              <p className="font-medium">{s.title}</p>
              {s.detail ? <p className="text-muted-foreground">{s.detail}</p> : null}
              {open ? null : (
                <Link to="/machines/add" className="text-sm underline underline-offset-2">
                  Add machines
                </Link>
              )}
            </div>
          ),
        )}
        <dl className="grid grid-cols-[5rem_1fr] gap-x-3 gap-y-1">
          {plan.affects ? (
            <>
              <dt className="text-muted-foreground">Affects</dt>
              <dd>{plan.affects}</dd>
            </>
          ) : null}
          {plan.cost ? (
            <>
              <dt className="text-muted-foreground">Cost</dt>
              <dd>{plan.cost}</dd>
            </>
          ) : null}
          <dt className="text-muted-foreground">Undo</dt>
          <dd className={plan.reversible ? "" : "font-medium"}>{plan.undo || (plan.reversible ? "Can be reverted from Change history." : "This cannot be undone.")}</dd>
        </dl>
        {plan.untrusted ? <p className="text-sm text-muted-foreground">{plan.untrusted}</p> : null}
        {showOnce.map((v) => (
          <ShowOnce key={v.label} title={v.label} label={v.label} secret={v.value} onDismiss={() => setShowOnce((all) => all.filter((x) => x !== v))} />
        ))}
        {error ? (
          <p role="alert" className="text-sm text-destructive">
            Nothing changed. {error}
          </p>
        ) : null}
        {asked ? <p role="status">{asked}</p> : null}
      </CardContent>
      <CardFooter className="flex-col items-start gap-2 border-t bg-muted/50 py-3">
        {plan.status === "applied" ? (
          <p role="status" className="flex flex-wrap items-center gap-2">
            <StatusBadge tone="good">Applied</StatusBadge>
            {plan.applied_at ? <span className="text-muted-foreground">{age(plan.applied_at)} ago</span> : null}
            {plan.pr_url ? (
              <a href={plan.pr_url} target="_blank" rel="noreferrer" className="underline underline-offset-2">
                Pull request
              </a>
            ) : null}
            <Link to="/settings/history" className="underline underline-offset-2">
              Change history
            </Link>
          </p>
        ) : null}
        {plan.status === "canceled" || plan.status === "expired" || plan.status === "reverted" ? (
          <StatusBadge>{{ canceled: "Canceled", expired: "Expired", reverted: "Reverted" }[plan.status]}</StatusBadge>
        ) : null}
        {open ? (
          <div className="flex flex-wrap items-center gap-2">
            {plan.stale ? (
              <Button
                disabled={busy}
                onClick={() =>
                  void run(async () => {
                    const next = await api.planConfig(plan.title, plan.changes.map((c) => ({ kind: c.kind, name: c.name, spec: c.new, delete: c.op === "delete" || undefined })), plan.request)
                    onRebuilt?.(next)
                    set(next)
                  })
                }
              >
                Rebuild plan
              </Button>
            ) : plan.can_confirm ? (
              phone ? (
                <p className="text-sm text-muted-foreground">This lowers protection, so it cannot be confirmed from a phone. Open it on a computer.</p>
              ) : (
                <Button disabled={busy || needsSecret} onClick={() => (lowers ? setSecond(true) : void apply())}>
                  Confirm
                </Button>
              )
            ) : plan.status === "awaiting_approval" ? (
              <StatusBadge tone="attention">Waiting for approval</StatusBadge>
            ) : (
              <Button
                disabled={busy}
                onClick={() =>
                  void run(async () => {
                    const r = await api.requestApproval(plan.id)
                    setAsked(r.message)
                    set(r.plan)
                  })
                }
              >
                Ask for approval
              </Button>
            )}
            {plan.stale ? <span className="text-sm text-muted-foreground">The configuration changed since this plan was made. Nothing stale is applied.</span> : null}
            {needsSecret && plan.can_confirm && !plan.stale ? <span className="text-sm text-muted-foreground">Fill the secure field first.</span> : null}
            {onEdit ? (
              <Button variant="outline" disabled={busy} onClick={onEdit}>
                Edit
              </Button>
            ) : null}
            {plan.changes.length > 0 ? <YamlDialog title={plan.title} yaml={planYaml(plan)} /> : null}
            <Button variant="ghost" disabled={busy} onClick={() => void run(async () => set(await api.cancelPlan(plan.id)))}>
              {cancelLabel}
            </Button>
          </div>
        ) : null}
        <p className="text-xs text-muted-foreground">
          {open ? `Valid ${until(plan.expires_at).replace(/^in /, "for ")}` : ""}
          {open && plan.needs.length > 0 ? " · " : ""}
          {plan.needs.length > 0 ? `needs: ${plan.needs.map(permissionWords).join(", ")}` : ""}
        </p>
      </CardFooter>
      {lowers ? <SecondConfirmation plan={plan} open={second} busy={busy} onClose={() => setSecond(false)} onConfirm={(w) => void apply(w)} /> : null}
    </Card>
  )
}
