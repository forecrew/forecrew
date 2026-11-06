import { useQuery, useQueryClient } from "@tanstack/react-query"
import { useNavigate, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { ApiError, api } from "@/api/client"
import { keys } from "@/api/queries"
import { Field } from "@/components/Field"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { safeRedirect } from "@/lib/redirect"

/** Sign in through a connection, or, where the server allows it, with a pasted token. */
export function LoginPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { redirect, org, invite } = useSearch({ from: "/login" })
  const discovery = useQuery({ queryKey: ["discover", org, invite], queryFn: () => api.discover({ org, invite }), retry: false })
  const [token, setToken] = useState("")
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!token.trim()) {
      setError("Paste your API token.")
      return
    }
    setBusy(true)
    setError("")
    try {
      const session = await api.login(token.trim())
      setToken("") // the token is never kept in the page once the cookie is set
      qc.setQueryData(keys.me, session)
      await navigate({ href: safeRedirect(redirect) })
    } catch (err) {
      setError(err instanceof ApiError && err.status === 401 ? "That token was not accepted. Check it, or create a new one." : (err as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="mx-auto flex min-h-screen max-w-md items-center p-4">
      <Card className="w-full">
        <CardHeader>
          <h1 className="text-2xl font-semibold tracking-tight">Log in to Forecrew</h1>
          <CardDescription>Sign in with your organization's account.</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {discovery.data?.connections.map((c) => (
            <a key={c.id} href={c.login_url} className={buttonVariants({ variant: "outline", className: "w-full" })}>
              Sign in with {c.name}
            </a>
          ))}
          {discovery.isError && (
            <div role="alert" className="space-y-2 text-sm">
              <p>The sign-in options could not be loaded.</p>
              <Button type="button" variant="outline" onClick={() => void discovery.refetch()}>
                Try again
              </Button>
            </div>
          )}
          {discovery.data?.connections.length === 0 && !discovery.data.token_login && (
            <p role="alert" className="text-sm">
              No way to sign in is set up on this server yet. Ask your administrator to add a connection.
            </p>
          )}
          {discovery.data?.token_login && (
          <form onSubmit={(e) => void submit(e)} className="space-y-4" noValidate>
            <Field label="API token" error={error}>
              {(p) => (
                <Input {...p} type="password" autoComplete="off" spellCheck={false} className="font-mono" value={token} onChange={(e) => setToken(e.target.value)} />
              )}
            </Field>
            <Button type="submit" disabled={busy} className="w-full">
              {busy ? "Logging in…" : "Log in"}
            </Button>
          </form>
          )}
        </CardContent>
      </Card>
    </main>
  )
}
