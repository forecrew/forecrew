import { useQueryClient } from "@tanstack/react-query"
import { useNavigate } from "@tanstack/react-router"
import { api } from "@/api/client"
import { keys, useChange, useIdentities, useMe, useRevokeToken, useSessions, useTokens } from "@/api/queries"
import type { Token } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { ConfirmDialog } from "@/components/ConfirmDialog"
import { CreateToken } from "@/components/CreateToken"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"

/** "7 days" for the Go duration "168h0m0s". */
export function capText(ttl: string): string {
  const hours = Number(/^(\d+)h/.exec(ttl)?.[1] ?? NaN)
  if (Number.isNaN(hours)) return ttl
  return hours % 24 === 0 ? `${hours / 24} days` : `${hours} hours`
}

/** A "Revoke" button that asks for confirmation; revoking the token in use logs you out. */
function RevokeToken({ token }: { token: Token }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  return (
    <ConfirmDialog
      trigger="Revoke"
      triggerAriaLabel={`Revoke ${token.name}`}
      title={`Revoke ${token.name}?`}
      description="The token and the web sessions it backs stop working at once. This cannot be undone."
      warning={token.current ? "This is the token you are using. You will be logged out." : undefined}
      confirmLabel={`Revoke ${token.name}`}
      keepLabel="Keep token"
      mutation={useRevokeToken()}
      variables={token.id}
      onSuccess={async (res) => {
        if (!res.self) return
        // The session is dead now: clear the cookie if we can, then start over.
        await api.logout().catch(() => {})
        qc.clear()
        await navigate({ to: "/login" })
      }}
    />
  )
}

/** Scopes in words; a token from before scopes has none recorded. */
export function Scopes({ token }: { token: Token }) {
  if (!token.scopes?.length) return <span className="text-muted-foreground">not recorded</span>
  return (
    <>
      {token.scopes.map((s) => (
        <StatusBadge key={s} className="mr-1">
          {s}
        </StatusBadge>
      ))}
    </>
  )
}

const expired = (token: Token) => Boolean(token.expires_at) && Date.parse(token.expires_at!) <= Date.now()

export function Expiry({ token }: { token: Token }) {
  if (!token.expires_at) return <span className="text-muted-foreground">never</span>
  if (expired(token)) return <StatusBadge tone="bad">expired</StatusBadge>
  return <Age iso={token.expires_at} future />
}

export function TokenTable({ items, label }: { items: Token[]; label: string }) {
  return (
    <TableWrap label={label}>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Scopes</TableHead>
            <TableHead>Expires</TableHead>
            <TableHead>Last used</TableHead>
            <TableHead>Revoked</TableHead>
            <TableHead>Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((t) => (
            <TableRow key={t.id}>
              <TableCell>
                <span className="font-mono">{t.name}</span> {t.current ? <StatusBadge tone="good">this session</StatusBadge> : null}
              </TableCell>
              <TableCell>
                <Scopes token={t} />
              </TableCell>
              <TableCell>
                <Expiry token={t} />
              </TableCell>
              <TableCell>
                <Age iso={t.last_used_at} ago />
              </TableCell>
              <TableCell>
                {t.revoked_at ? (
                  <span className="inline-flex items-center gap-2">
                    <StatusBadge tone="bad">revoked</StatusBadge> <Age iso={t.revoked_at} ago />
                  </span>
                ) : (
                  <span className="text-muted-foreground">no</span>
                )}
              </TableCell>
              <TableCell>{t.revoked_at ? null : <RevokeToken token={t} />}</TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableWrap>
  )
}

function Section({ id, title, children }: { id: string; title: string; children: React.ReactNode }) {
  return (
    <section aria-labelledby={id} className="space-y-3">
      <h2 id={id} className="text-lg font-semibold">
        {title}
      </h2>
      {children}
    </section>
  )
}

/** Sign-ins, personal tokens and sessions of the person signed in. */
export function AccountPage() {
  const me = useMe().data
  const identities = useIdentities()
  const tokens = useTokens()
  const sessions = useSessions()
  const end = useChange((id: string) => api.endSession(id), keys.sessions)
  const navigate = useNavigate()
  const qc = useQueryClient()
  const person = me?.user
  // list_tokens shows everyone's to a token manager: this page is about yours.
  const mine = (all: Token[]) => all.filter((t) => t.owner_kind !== "service_account" && (!person || !t.owner_id || t.owner_id === person.id))

  const signOutEverywhere = async () => {
    try {
      await api.signOutEverywhere()
    } finally {
      qc.clear()
      await navigate({ to: "/login" })
    }
  }

  return (
    <div className="space-y-8">
      <PageHeader title="Your account" meta={person ? `${person.name}${person.email ? ` · ${person.email}` : ""}` : me?.token ? `Token ${me.token}` : undefined} />

      <Section id="signins" title="Sign-ins">
        {person ? (
          <Async query={identities} isEmpty={(d) => d.length === 0} empty="No sign-in is linked to your account.">
            {(items) => (
              <TableWrap label="Sign-ins">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Sign-in</TableHead>
                      <TableHead>As</TableHead>
                      <TableHead>Last used</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {items.map((i) => (
                      <TableRow key={i.id}>
                        <TableCell>{i.connection}</TableCell>
                        <TableCell>{i.login || i.email || "—"}</TableCell>
                        <TableCell>
                          <Age iso={i.last_used_at} ago />
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </TableWrap>
            )}
          </Async>
        ) : (
          <p className="text-sm text-muted-foreground">You use a token, which is not linked to a sign-in.</p>
        )}
      </Section>

      <Section id="tokens" title="Personal tokens">
        <p className="text-sm text-muted-foreground">
          For scripts, the CLI and MCP clients. A token can do no more than you can, and stops working when you leave the organization.
          {me?.account?.max_token_ttl ? ` Your organization requires company sign-in, which limits personal tokens to ${capText(me.account.max_token_ttl)}.` : ""}
        </p>
        <CreateToken />
        <Async query={tokens} isEmpty={(d) => mine(d).length === 0} empty="No tokens.">
          {(items) => <TokenTable items={mine(items)} label="Tokens" />}
        </Async>
      </Section>

      <Section id="sessions" title="Sessions">
        <Async query={sessions} isEmpty={(d) => d.length === 0} empty="No browser sessions.">
          {(items) => (
            <TableWrap label="Sessions">
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Browser</TableHead>
                    <TableHead>Organization</TableHead>
                    <TableHead>Signed in</TableHead>
                    <TableHead>Last used</TableHead>
                    <TableHead>Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {items.map((s) => (
                    <TableRow key={s.id}>
                      <TableCell>
                        {s.user_agent || "Unknown browser"} {s.ip ? <span className="text-muted-foreground">{s.ip}</span> : null}{" "}
                        {s.current ? <StatusBadge tone="good">this session</StatusBadge> : null}
                      </TableCell>
                      <TableCell>{s.organization || "—"}</TableCell>
                      <TableCell>
                        <Age iso={s.created_at} ago />
                      </TableCell>
                      <TableCell>
                        <Age iso={s.last_seen_at} ago />
                      </TableCell>
                      <TableCell>
                        {s.current ? null : (
                          <Button variant="outline" aria-label={`End session, ${s.user_agent || "an unknown browser"}`} onClick={() => end.mutate(s.id)} disabled={end.isPending}>
                            End session
                          </Button>
                        )}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </TableWrap>
          )}
        </Async>
        {end.isError ? (
          <p role="alert" className="text-sm text-destructive">
            {end.error.message}
          </p>
        ) : null}
        <Button variant="outline" onClick={() => void signOutEverywhere()}>
          Sign out everywhere
        </Button>
      </Section>
    </div>
  )
}
