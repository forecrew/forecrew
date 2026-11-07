import { useState } from "react"
import { api } from "@/api/client"
import { keys, useChange, useMembers } from "@/api/queries"
import type { UnmappedPerson } from "@/api/types"
import { PermButton, useCan } from "@/lib/permissions"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"

/** One person on the tracker who handed tickets over and is not a Forecrew user yet: a suggestion to confirm, or a member to choose. */
function Person({ p }: { p: UnmappedPerson }) {
  const can = useCan()
  const members = useMembers(can("org.read"))
  const [user, setUser] = useState("")
  const map = useChange(
    (u: string) => api.mapTrackerIdentity({ connection: p.connection, external_id: p.external_id, external_name: p.external_name, user: u }),
    keys.trackerUnmapped, keys.trackerIdentities, keys.tickets,
  )
  const name = p.external_name || p.external_id
  return (
    <li className="space-y-2 rounded-lg border bg-card p-3 text-sm">
      <p>
        <span className="font-medium">{name}</span> <span className="font-mono text-xs text-muted-foreground">{p.external_id}</span>
        <span className="text-muted-foreground">
          {" "}
          · {p.tickets.length} {p.tickets.length === 1 ? "ticket" : "tickets"}: {p.tickets.join(", ")}
        </span>
      </p>
      <div className="flex flex-wrap items-center gap-2">
        {p.suggestions.map((s) => (
          <PermButton key={s.user} permission="trackers.manage" variant="outline" disabled={map.isPending} onClick={() => map.mutate(s.user)}>
            Same person as {s.name} ({s.reason})
          </PermButton>
        ))}
        {p.suggestions.length === 0 && members.data ? (
          <>
            <NativeSelect aria-label={`Forecrew member for ${name}`} value={user} onChange={(e) => setUser(e.target.value)}>
              <NativeSelectOption value="">Choose a member</NativeSelectOption>
              {members.data.filter((m) => m.status === "active").map((m) => (
                <NativeSelectOption key={m.user_id} value={m.user_id}>
                  {m.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
            <PermButton permission="trackers.manage" variant="outline" aria-label={`Match ${name} to the chosen member`} disabled={!user || map.isPending} onClick={() => map.mutate(user)}>
              Match
            </PermButton>
          </>
        ) : null}
      </div>
      {map.isError ? (
        <p role="alert" className="text-xs text-destructive">
          {map.error.message}
        </p>
      ) : null}
    </li>
  )
}

/** Who on a connection is not matched to a Forecrew user yet. A person confirms every match: the match lets that tracker user act as the member. */
export function PeopleReview({ people }: { people: UnmappedPerson[] }) {
  return (
    <div className="space-y-2">
      <p className="text-sm text-muted-foreground">Matching someone lets whatever they hand over on the tracker start goals as that member, so you confirm each one.</p>
      <ul aria-label="People not matched yet" className="space-y-2">
        {people.map((p) => (
          <Person key={`${p.connection}/${p.external_id}`} p={p} />
        ))}
      </ul>
    </div>
  )
}
