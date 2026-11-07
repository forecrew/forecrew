import { useNavigate, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { api } from "@/api/client"
import { useMe } from "@/api/queries"
import type { CliAuthorization } from "@/api/types"
import { PageHeader } from "@/components/PageHeader"
import { ShowOnce } from "@/components/ShowOnce"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { leave } from "@/lib/redirect"

/** Whether the CLI's address is on this machine: the server refuses any other. */
export function isLoopback(uri: string | undefined): boolean {
  try {
    const u = new URL(uri ?? "")
    return u.protocol === "http:" && (u.hostname === "127.0.0.1" || u.hostname === "[::1]") && u.port !== ""
  } catch {
    return false
  }
}

/** Where the command line asks to be allowed to act as you; you confirm, and the browser goes back to it. */
export function CliLoginPage() {
  const me = useMe().data
  const navigate = useNavigate()
  const q = useSearch({ strict: false }) as { redirect_uri?: string; state?: string; code_challenge?: string; organization?: string; host?: string }
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState<CliAuthorization>()
  const valid = isLoopback(q.redirect_uri) && Boolean(q.code_challenge)
  const org = q.organization || me?.account?.slug

  const allow = async () => {
    setBusy(true)
    setError("")
    try {
      const out = await api.cliAuthorize({ redirect_uri: q.redirect_uri!, state: q.state ?? "", code_challenge: q.code_challenge!, organization: q.organization })
      setDone(out)
      leave.to(out.redirect)
    } catch (err) {
      setError((err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  if (!valid) {
    return (
      <>
        <PageHeader title="Allow the command line" />
        <p role="alert" className="text-sm">
          This address is not a login that <code className="font-mono">forecrew login</code> started. Run the command again.
        </p>
      </>
    )
  }
  return (
    <div className="mx-auto max-w-lg space-y-4">
      <PageHeader title="Allow the command line" />
      <Card>
        <CardHeader>
          <p>
            <span className="font-mono">forecrew</span> on <strong>{q.host || "a machine"}</strong> asks to act as {me?.user?.name ?? "you"} in <strong>{org}</strong>.
          </p>
        </CardHeader>
        <CardContent className="space-y-3 text-sm">
          <p className="text-muted-foreground">
            It gets a personal token that can do no more than you can. You can revoke it in Your account at any time. Allow it only if you just ran{" "}
            <code className="font-mono">forecrew login</code>.
          </p>
          {error ? (
            <p role="alert" className="text-destructive">
              {error}
            </p>
          ) : null}
          <div className="flex gap-2">
            <Button onClick={() => void allow()} disabled={busy || Boolean(done)}>
              Allow
            </Button>
            <Button variant="outline" onClick={() => void navigate({ to: "/" })}>
              Cancel
            </Button>
          </div>
        </CardContent>
      </Card>
      {done ? (
        <>
          <p role="status" className="text-sm">
            Allowed. This window goes back to the command line. If it does not, paste this code into the terminal.
          </p>
          <ShowOnce title="Login code" label="Code" secret={done.code} onDismiss={() => setDone(undefined)} />
        </>
      ) : null}
    </div>
  )
}
