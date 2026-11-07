import { useState } from "react"
import { api } from "@/api/client"
import { keys, useChange, useServiceAccounts, useTokens } from "@/api/queries"
import type { ServiceAccount } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { CreateToken } from "@/components/CreateToken"
import { Field } from "@/components/Field"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { PermButton, useCan } from "@/lib/permissions"
import { Expiry, RevokeToken, Scopes } from "../account"

function CreateAccount() {
  const can = useCan()
  const [open, setOpen] = useState(false)
  const [name, setName] = useState("")
  const [description, setDescription] = useState("")
  const [error, setError] = useState("")
  const create = useChange((v: { name: string; description: string }) => api.createServiceAccount({ name: v.name, description: v.description || undefined }), keys.serviceAccounts)
  if (!can("service_account.manage")) return <PermButton permission="service_account.manage">Add service account</PermButton>
  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) create.reset()
      }}
    >
      <DialogTrigger render={<Button />}>Add service account</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Add a service account</DialogTitle>
          <DialogDescription>For CI and scripts. It holds tokens and roles, and never signs in.</DialogDescription>
        </DialogHeader>
        <form
          noValidate
          className="space-y-3"
          onSubmit={(e) => {
            e.preventDefault()
            if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(name)) {
              setError("Use lowercase letters, digits, dots, dashes and underscores.")
              return
            }
            setError("")
            create.mutate({ name, description }, { onSuccess: () => { setOpen(false); setName(""); setDescription("") } })
          }}
        >
          <Field label="Name" error={error}>
            {(p) => <Input {...p} className="font-mono" value={name} onChange={(e) => setName(e.target.value)} />}
          </Field>
          <Field label="What it is for">{(p) => <Input {...p} value={description} onChange={(e) => setDescription(e.target.value)} />}</Field>
          {create.isError ? (
            <p role="alert" className="text-sm text-destructive">
              {create.error.message}
            </p>
          ) : null}
          <DialogFooter>
            <Button type="submit" disabled={create.isPending}>
              Add service account
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function AccountCard({ account }: { account: ServiceAccount }) {
  const can = useCan()
  const manage = can("service_account.manage")
  const tokens = useTokens(can("token.manage"))
  const toggle = useChange((disabled: boolean) => api.setServiceAccountDisabled(account.name, disabled), keys.serviceAccounts)
  const own = (tokens.data ?? []).filter((t) => t.owner_kind === "service_account" && t.owner_id === account.id)
  return (
    <Card role="region" aria-label={`Service account ${account.name}`}>
      <CardHeader className="flex flex-row flex-wrap items-center gap-2">
        <h3 className="font-mono font-medium">{account.name}</h3>
        {account.disabled_at ? <StatusBadge tone="attention">Disabled</StatusBadge> : <StatusBadge tone="good">Active</StatusBadge>}
        <span className="flex-1 text-sm text-muted-foreground">{account.description}</span>
        {manage ? (
          <Button variant="outline" aria-label={`${account.disabled_at ? "Enable" : "Disable"} ${account.name}`} onClick={() => toggle.mutate(!account.disabled_at)} disabled={toggle.isPending}>
            {account.disabled_at ? "Enable" : "Disable"}
          </Button>
        ) : null}
      </CardHeader>
      <CardContent className="space-y-3">
        {toggle.isError ? (
          <p role="alert" className="text-sm text-destructive">
            {toggle.error.message}
          </p>
        ) : null}
        {can("token.manage") ? (
          <>
            <h4 className="text-sm font-medium">Tokens</h4>
            {own.length === 0 ? (
              <p className="text-sm text-muted-foreground">No tokens.</p>
            ) : (
              <ul className="space-y-1 text-sm">
                {own.map((t) => (
                  <li key={t.id} className="flex flex-wrap items-center gap-2">
                    <span className="font-mono">{t.name}</span> <Scopes token={t} /> <Expiry token={t} />
                    {t.revoked_at ? <StatusBadge tone="bad">revoked</StatusBadge> : <RevokeToken token={t} />}
                    <span className="text-muted-foreground">
                      used <Age iso={t.last_used_at} ago />
                    </span>
                  </li>
                ))}
              </ul>
            )}
            {!account.disabled_at ? <CreateToken serviceAccount={account.name} /> : null}
          </>
        ) : null}
      </CardContent>
    </Card>
  )
}

export function ServiceAccountsTab() {
  const accounts = useServiceAccounts()
  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <CreateAccount />
      </div>
      <Async query={accounts} isEmpty={(d) => d.length === 0} empty="Service accounts run CI and scripts without a person. Add one when you need it.">
        {(items) => (
          <div className="space-y-4">
            {items.map((a) => (
              <AccountCard key={a.id} account={a} />
            ))}
          </div>
        )}
      </Async>
    </div>
  )
}
