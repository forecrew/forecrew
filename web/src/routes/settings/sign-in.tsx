import { LockIcon } from "lucide-react"
import { useState } from "react"
import { api } from "@/api/client"
import { keys, useChange, useConnections } from "@/api/queries"
import type { Connection, ConnectionKind } from "@/api/types"
import { Async } from "@/components/Async"
import { Field } from "@/components/Field"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Button, buttonVariants } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Section } from "./layout"

const KINDS: { value: ConnectionKind; label: string }[] = [
  { value: "github", label: "GitHub" },
  { value: "google", label: "Google" },
  { value: "microsoft", label: "Microsoft Entra ID" },
  { value: "oidc", label: "Other OpenID Connect (Okta, Auth0, ...)" },
]
const kindLabel = (k: ConnectionKind) => KINDS.find((x) => x.value === k)?.label ?? k

/** Adds a company sign-in. It starts off: sign in through it once, then turn it on. */
function AddConnection({ onAdded }: { onAdded: (c: Connection) => void }) {
  const [open, setOpen] = useState(false)
  const [kind, setKind] = useState<ConnectionKind>("oidc")
  const [name, setName] = useState("")
  const [issuer, setIssuer] = useState("")
  const [clientId, setClientId] = useState("")
  const [secret, setSecret] = useState("")
  const [errors, setErrors] = useState<Record<string, string>>({})
  // The client secret goes through a change plan: a secure field straight to
  // the secret store, and the connection starts turned off.
  const add = useChange(async (v: { kind: ConnectionKind; name: string; issuer?: string; client_id: string; secret: string }) => {
    const ref = `conn-${v.name}`
    const spec = { kind: v.kind, client_id: v.client_id, client_secret_ref: `store:${ref}`, jit: { enabled: false }, enabled: false, ...(v.issuer ? { issuer: v.issuer } : {}) }
    const plan = await api.planConfig(`Add sign-in ${v.name}`, [{ kind: "connection", name: v.name, spec }])
    try {
      await api.putPlanSecret(plan.id, ref, v.secret)
      await api.applyPlan(plan.id)
    } catch (err) {
      await api.cancelPlan(plan.id).catch(() => {}) // no plan is left open
      throw err
    }
    return (await api.listConnections()).find((c) => c.name === v.name)
  }, keys.connections)
  const needsIssuer = kind === "oidc" || kind === "microsoft"
  // Nothing typed stays in the page once the dialog is closed or the form is done.
  const clear = () => {
    setName("")
    setIssuer("")
    setClientId("")
    setSecret("")
    setErrors({})
    add.reset()
  }

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    const next: Record<string, string> = {}
    if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(name)) next.name = "Use lowercase letters, digits, dots, dashes and underscores."
    if (needsIssuer && !/^https:\/\/\S+$/.test(issuer)) next.issuer = "Enter the https address of the issuer."
    if (!clientId.trim()) next.clientId = "Enter the client ID from your provider."
    if (!secret.trim()) next.secret = "Paste the client secret from your provider."
    setErrors(next)
    if (Object.keys(next).length) return
    add.mutate(
      { kind, name, client_id: clientId.trim(), secret, ...(needsIssuer ? { issuer } : {}) },
      {
        onSuccess: (c) => {
          setOpen(false)
          clear()
          if (c) onAdded(c)
        },
      },
    )
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) clear()
      }}
    >
      <DialogTrigger render={<Button />}>Add company sign-in</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add company sign-in</DialogTitle>
          <DialogDescription>It starts turned off. Sign in through it once to test it, then turn it on.</DialogDescription>
        </DialogHeader>
        <form onSubmit={submit} noValidate className="space-y-3">
          <Field label="Provider">
            {(p) => (
              <NativeSelect {...p} value={kind} onChange={(e) => setKind(e.target.value as ConnectionKind)}>
                {KINDS.map((k) => (
                  <NativeSelectOption key={k.value} value={k.value}>
                    {k.label}
                  </NativeSelectOption>
                ))}
              </NativeSelect>
            )}
          </Field>
          <Field label="Name" error={errors.name} hint="Shown on the sign-in button.">
            {(p) => <Input {...p} className="font-mono" value={name} onChange={(e) => setName(e.target.value)} />}
          </Field>
          {needsIssuer ? (
            <Field label="Issuer" error={errors.issuer} hint="For example https://acme.okta.com">
              {(p) => <Input {...p} value={issuer} onChange={(e) => setIssuer(e.target.value)} />}
            </Field>
          ) : null}
          <Field label="Client ID" error={errors.clientId}>
            {(p) => <Input {...p} className="font-mono" value={clientId} onChange={(e) => setClientId(e.target.value)} />}
          </Field>
          <Field label="Client secret" error={errors.secret} hint="A secure field: it goes straight to the server's secret store and is never shown again.">
            {(p) => (
              <div className="flex items-center gap-2">
                <LockIcon className="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                <Input {...p} type="password" autoComplete="off" spellCheck={false} className="font-mono" value={secret} onChange={(e) => setSecret(e.target.value)} />
              </div>
            )}
          </Field>
          {add.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {add.error.message}
            </p>
          ) : null}
          <DialogFooter>
            <Button type="submit" disabled={add.isPending}>
              Add sign-in
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function ConnectionActions({ c }: { c: Connection }) {
  const set = useChange((enabled: boolean) => api.setConnectionEnabled(c.name, enabled), keys.connections)
  if (c.instance) return <StatusBadge>Managed by server file</StatusBadge>
  return (
    <div className="flex flex-wrap items-start gap-2">
      {c.test_url ? (
        <a href={c.test_url} target="_blank" rel="noreferrer" className={buttonVariants({ variant: "outline" })} aria-label={`Test sign-in with ${c.name}`}>
          Test sign-in
        </a>
      ) : null}
      {c.enabled ? (
        <Button variant="outline" aria-label={`Turn off ${c.name}`} onClick={() => set.mutate(false)} disabled={set.isPending}>
          Turn off
        </Button>
      ) : (
        <span className="inline-flex flex-col items-start gap-0.5">
          <Button variant="outline" aria-label={`Turn on ${c.name}`} onClick={() => set.mutate(true)} disabled={set.isPending || !c.tested}>
            Turn on
          </Button>
          {!c.tested ? <span className="text-xs text-muted-foreground">Test the sign-in first.</span> : null}
        </span>
      )}
      {set.isError ? (
        <p role="alert" className="w-full text-xs text-destructive">
          {set.error.message}
        </p>
      ) : null}
    </div>
  )
}

/** The ways to sign in: the server's, which are read-only, and the organization's own. */
export function SignInPage() {
  const connections = useConnections()
  const [added, setAdded] = useState<Connection>()
  return (
    <Section permission="connection.manage" title="Sign-in" meta="People sign in with the ways below." actions={<AddConnection onAdded={setAdded} />}>
      {added ? (
        <div role="status" className="mb-4 rounded-xl border bg-card p-4 text-sm">
          <p className="font-medium">{added.name} is added, turned off.</p>
          <p className="mt-1">
            Allow this redirect address at your provider: <code className="font-mono break-all">{added.redirect_url}</code>
          </p>
          <p className="mt-1">Then use Test sign-in on its row, and turn it on.</p>
        </div>
      ) : null}
      <Async query={connections} isEmpty={(d) => d.length === 0} empty="No way to sign in is set up yet.">
        {(items) => (
          <TableWrap label="Ways to sign in">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Sign-in</TableHead>
                  <TableHead>Address</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Redirect address</TableHead>
                  <TableHead>Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((c) => (
                  <TableRow key={c.id}>
                    <TableCell>
                      <div className="font-medium">{c.name}</div>
                      <div className="text-sm text-muted-foreground">{kindLabel(c.kind)}</div>
                    </TableCell>
                    <TableCell>{c.instance && !c.issuer ? "All organizations on this server" : c.issuer || "—"}</TableCell>
                    <TableCell>
                      <StatusBadge tone={c.enabled ? "good" : "neutral"}>{c.enabled ? "On" : "Off"}</StatusBadge>{" "}
                      {!c.instance ? <StatusBadge tone={c.tested ? "good" : "attention"}>{c.tested ? "Tested" : "Not tested"}</StatusBadge> : null}
                    </TableCell>
                    <TableCell className="max-w-64 break-all font-mono text-xs">{c.redirect_url}</TableCell>
                    <TableCell>
                      <ConnectionActions c={c} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        )}
      </Async>
    </Section>
  )
}
