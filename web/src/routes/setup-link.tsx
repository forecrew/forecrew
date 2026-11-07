import { useQuery } from "@tanstack/react-query"
import { useLocation, useNavigate } from "@tanstack/react-router"
import { useEffect, useState } from "react"
import { api } from "@/api/client"
import { buttonVariants } from "@/components/ui/button"
import { Card, CardContent, CardDescription, CardHeader } from "@/components/ui/card"

type Checked = { status: "checking" } | { status: "ready"; organization: string } | { status: "bad"; message: string }

/**
 * The one-time setup link the installer prints: the token is in the address
 * after the #, so no server log or referrer holds it. The page takes it out of
 * the address, has the server check it, and then offers the sign-in; whoever
 * signs in next becomes the first Owner.
 */
export function SetupLinkPage() {
  const navigate = useNavigate()
  const hash = useLocation({ select: (l) => l.hash })
  // Kept apart from the address, which is cleared at once.
  const [token] = useState(() => hash.replace(/^#/, ""))
  const [checked, setChecked] = useState<Checked>({ status: "checking" })

  useEffect(() => {
    void navigate({ to: "/setup-link", hash: "", replace: true })
    if (!token) return
    let cancelled = false
    api.redeemSetupLink(token).then(
      (r) => !cancelled && setChecked({ status: "ready", organization: r.organization }),
      (err: Error) => !cancelled && setChecked({ status: "bad", message: err.message }),
    )
    return () => {
      cancelled = true
    }
  }, [token, navigate])

  // Without a token there is nothing to check.
  const shown: Checked = token ? checked : { status: "bad", message: "This page needs the link the installer printed, with everything after the #." }
  const options = useQuery({ queryKey: ["discover", "setup-link"], queryFn: () => api.discover({}), retry: false, enabled: shown.status === "ready" })

  return (
    <main className="mx-auto flex min-h-screen max-w-md items-center p-4">
      <Card className="w-full">
        <CardHeader>
          <h1 className="text-2xl font-semibold tracking-tight">Set up Forecrew</h1>
          <CardDescription>The first person to sign in with this link becomes the Owner.</CardDescription>
        </CardHeader>
        <CardContent className="space-y-4">
          {shown.status === "checking" ? <p role="status" className="text-sm text-muted-foreground">Checking the link…</p> : null}
          {shown.status === "bad" ? (
            <p role="alert" className="text-sm">
              {shown.message}
            </p>
          ) : null}
          {shown.status === "ready" ? (
            <>
              <p className="text-sm">
                Sign in to become the Owner of <span className="font-medium">{shown.organization}</span>. The link works once and stops working after a short while.
              </p>
              {options.data?.connections.map((c) => (
                <a key={c.id} href={c.login_url} className={buttonVariants({ variant: "outline", className: "w-full" })}>
                  Sign in with {c.name}
                </a>
              ))}
              {options.isError ? (
                <p role="alert" className="text-sm">
                  The sign-in options could not be loaded. Reload the page, and if it stays so, create a new link on the server.
                </p>
              ) : null}
              {options.data && options.data.connections.length === 0 ? (
                <p role="alert" className="text-sm">
                  No way to sign in is set up on this server yet. Add a connection to forecrew.yaml, restart, and create a new link.
                </p>
              ) : null}
            </>
          ) : null}
        </CardContent>
      </Card>
    </main>
  )
}
