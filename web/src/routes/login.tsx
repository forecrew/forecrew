import { useQuery, useQueryClient } from "@tanstack/react-query"
import { useNavigate, useSearch } from "@tanstack/react-router"
import { useEffect, useState } from "react"
import { ApiError, api } from "@/api/client"
import { keys } from "@/api/queries"
import type { Discovery } from "@/api/types"
import { Field } from "@/components/Field"
import { Button, buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card"
import { Input } from "@/components/ui/input"
import { isAuthorization, leave, safeRedirect } from "@/lib/redirect"

/** The login address with the page to come back to; the server only goes to a path of this site. */
export function withReturn(loginUrl: string, redirect: string | undefined): string {
  if (!redirect) return loginUrl
  return `${loginUrl}${loginUrl.includes("?") ? "&" : "?"}redirect=${encodeURIComponent(safeRedirect(redirect))}`
}

/**
 * Sign in: an email first (the company's own sign-in shows when its domain
 * has one), one button per way to sign in, and, where the server allows it, a
 * pasted token.
 */
export function LoginPage() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const { redirect, org, invite } = useSearch({ from: "/login" })
  const [email, setEmail] = useState("")
  const [typed, setTyped] = useState("")
  const discovery = useQuery({
    queryKey: ["discover", org, invite, typed],
    queryFn: () => api.discover({ org, invite, ...(typed ? { email: typed } : {}) }),
    retry: false,
  })
  const [token, setToken] = useState("")
  const [error, setError] = useState("")
  const [emailError, setEmailError] = useState("")
  const [busy, setBusy] = useState(false)
  const found: Discovery | undefined = discovery.data

  const next = (e: React.FormEvent) => {
    e.preventDefault()
    if (!/^\S+@\S+\.\S+$/.test(email.trim())) {
      setEmailError("Enter your email address.")
      return
    }
    setEmailError("")
    setTyped(email.trim())
  }

  // The session cookie is not sent on a navigation that started on another
  // site, which is how an app sends people to the consent screen, so the
  // server sent them here. Already signed in, go back from this site.
  useEffect(() => {
    if (!isAuthorization(redirect)) return
    api.me().then(() => leave.to(redirect), () => {})
  }, [redirect])

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
      if (isAuthorization(redirect)) leave.to(redirect)
      else await navigate({ href: safeRedirect(redirect) })
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
          {invite ? <p className="text-sm">You were invited. Sign in with the account the invitation was sent to.</p> : null}
          <form onSubmit={next} className="space-y-3" noValidate>
            <Field label="Email" error={emailError} hint="Your company's own sign-in shows up when it has one.">
              {(p) => <Input {...p} type="email" autoComplete="email" value={email} onChange={(e) => setEmail(e.target.value)} />}
            </Field>
            <Button type="submit" variant="outline" className="w-full">
              Continue
            </Button>
          </form>
          {found?.redirect ? (
            <div role="status" className="space-y-2 rounded-lg border p-3 text-sm">
              <p>Your company requires its own sign-in for {typed}.</p>
              <a href={withReturn(found.redirect, redirect)} className={buttonVariants({ className: "w-full" })}>
                Continue with your company sign-in
              </a>
            </div>
          ) : null}
          {found?.connections.map((c) => (
            <a key={c.id} href={withReturn(c.login_url, redirect)} className={buttonVariants({ variant: "outline", className: "w-full" })}>
              Sign in with {c.name}
            </a>
          ))}
          {discovery.isError ? (
            <div role="alert" className="space-y-2 text-sm">
              <p>The sign-in options could not be loaded.</p>
              <Button type="button" variant="outline" onClick={() => void discovery.refetch()}>
                Try again
              </Button>
            </div>
          ) : null}
          {found && found.connections.length === 0 && !found.token_login ? (
            <p role="alert" className="text-sm">
              No way to sign in is set up on this server yet. Ask your administrator to add a connection.
            </p>
          ) : null}
          {found?.token_login ? (
            <form onSubmit={(e) => void submit(e)} className="space-y-4" noValidate>
              <Field label="API token" error={error}>
                {(p) => <Input {...p} type="password" autoComplete="off" spellCheck={false} className="font-mono" value={token} onChange={(e) => setToken(e.target.value)} />}
              </Field>
              <Button type="submit" disabled={busy} className="w-full">
                {busy ? "Logging in…" : "Log in"}
              </Button>
            </form>
          ) : null}
        </CardContent>
      </Card>
    </main>
  )
}
