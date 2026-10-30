import { useQueryClient } from "@tanstack/react-query"
import { useNavigate, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { ApiError, api } from "@/api/client"
import { keys } from "@/api/queries"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { safeRedirect } from "@/lib/redirect"

/** Paste an API token; the server answers with the session cookie. */
export function LoginPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { redirect } = useSearch({ from: "/login" })
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
          <CardDescription>
            Log in with an API token. Create one on the server with <code className="font-mono">forecrew token create</code>.
          </CardDescription>
        </CardHeader>
        <CardContent>
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
        </CardContent>
      </Card>
    </main>
  )
}
