import { useNavigate } from "@tanstack/react-router"
import { useQueryClient } from "@tanstack/react-query"
import { api } from "@/api/client"
import { useRevokeToken, useTokens } from "@/api/queries"
import type { Token } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { ConfirmDialog } from "@/components/ConfirmDialog"
import { ExternalLink } from "@/components/ExternalLink"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"

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

export function TokensPage() {
  const tokens = useTokens()
  return (
    <>
      <PageHeader title="Tokens" />
      <Async query={tokens} isEmpty={(d) => d.length === 0} empty="No tokens.">
        {(items) => (
          <TableWrap label="Tokens">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead>Last used</TableHead>
                  <TableHead>Revoked</TableHead>
                  <TableHead>Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((t) => (
                  <TableRow key={t.id}>
                    <TableCell>
                      <span className="font-mono">{t.name}</span>{" "}
                      {t.current ? <StatusBadge tone="good">this session</StatusBadge> : null}
                    </TableCell>
                    <TableCell>
                      <Age iso={t.created_at} ago />
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
        )}
      </Async>
      <p className="mt-6 text-sm text-muted-foreground">
        New tokens are created on the server with <code className="font-mono">forecrew token create</code>. See the{" "}
        <ExternalLink href="https://github.com/forecrew/forecrew/blob/main/docs/runbooks/mcp-token.md">MCP token runbook</ExternalLink>.
      </p>
    </>
  )
}
