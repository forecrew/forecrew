import { useState } from "react"
import { api } from "@/api/client"
import { keys, useChange, useProjects } from "@/api/queries"
import type { TicketInfo } from "@/api/types"
import { ExternalLink } from "@/components/ExternalLink"
import { StatusBadge } from "@/components/StatusBadge"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { PermButton } from "@/lib/permissions"
import { reasonWords, safeUrl } from "@/lib/trackers"

/** The key of a ticket, a link to it on its tracker when the address is a web one. Ticket text is plain text. */
export function TicketKey({ t }: { t: Pick<TicketInfo, "key" | "url"> }) {
  const url = safeUrl(t.url)
  return url ? <ExternalLink href={url}>{t.key}</ExternalLink> : <span className="font-mono">{t.key}</span>
}

function Ticket({ t }: { t: TicketInfo }) {
  const projects = useProjects()
  const [project, setProject] = useState("")
  const done = [keys.tickets, keys.trackerUnmapped, ["gates"], ["goals"]] as const
  const accept = useChange(() => api.acceptTicket(t.id, t.project ? undefined : project), ...done)
  const dismiss = useChange(() => api.dismissTicket(t.id), ...done)
  const queued = t.status === "queued"
  const needsProject = !queued && !t.project
  const unknown = t.reason === "unknown_person"
  return (
    <li className="space-y-2 rounded-lg border bg-card p-3 text-sm">
      <p className="flex flex-wrap items-center gap-2">
        <TicketKey t={t} />
        <span>{t.title}</span>
        {t.project ? <StatusBadge>{t.project}</StatusBadge> : null}
        <StatusBadge tone={queued ? "neutral" : "attention"}>{queued ? "Queued" : "Waiting for you"}</StatusBadge>
      </p>
      <p className="text-muted-foreground">{reasonWords(t)}</p>
      {!queued ? (
        <div className="flex flex-wrap items-center gap-2">
          {needsProject ? (
            <NativeSelect aria-label={`Project for ${t.key}`} value={project} onChange={(e) => setProject(e.target.value)}>
              <NativeSelectOption value="">Choose a project</NativeSelectOption>
              {projects.data?.filter((p) => p.status === "onboarded").map((p) => (
                <NativeSelectOption key={p.id} value={p.name}>
                  {p.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          ) : null}
          <PermButton permission="goal.create" project={t.project_id} aria-label={`${unknown ? "Start anyway" : "Accept"}: ${t.key}`} disabled={accept.isPending || (needsProject && !project)} onClick={() => accept.mutate(undefined)}>
            {unknown ? "Start anyway" : "Accept"}
          </PermButton>
          <PermButton permission="goal.create" project={t.project_id} variant="outline" aria-label={`Dismiss: ${t.key}`} disabled={dismiss.isPending} onClick={() => dismiss.mutate(undefined)}>
            Dismiss
          </PermButton>
        </div>
      ) : null}
      {accept.isError || dismiss.isError ? (
        <p role="alert" className="text-xs text-destructive">
          {(accept.error ?? dismiss.error)?.message}
        </p>
      ) : null}
    </li>
  )
}

/** The tickets of a connection that wait for a person or for a free slot, each with why. */
export function WaitingTickets({ tickets }: { tickets: TicketInfo[] }) {
  return (
    <ul aria-label="Tickets waiting" className="space-y-2">
      {tickets.map((t) => (
        <Ticket key={t.id} t={t} />
      ))}
    </ul>
  )
}
