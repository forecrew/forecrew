import { CheckIcon, TriangleAlertIcon, XIcon } from "lucide-react"
import { useState } from "react"
import { api } from "@/api/client"
import type { TicketInfo, TrackerConnection, TrackerRoute, TrackerTest, UnmappedPerson } from "@/api/types"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { Age } from "@/components/Age"
import { PermButton } from "@/lib/permissions"
import { addressOf, checkHelp, kindLabel, routeSentence, triggerWords } from "@/lib/trackers"
import { PeopleReview } from "./people"
import { AddRule } from "./rules"
import { WaitingTickets } from "./tickets"

/** The results of a connection test, one line each, with what to do about the ones that failed. Details can quote the tracker: plain text. */
function Results({ test, kind, children }: { test: TrackerTest; kind: string; children?: React.ReactNode }) {
  return (
    <div className="space-y-2">
      <ul aria-label={`Test of ${test.connection}`} className="space-y-1.5 text-sm">
        {test.checks.map((c) => (
          <li key={c.name} className="flex gap-2">
            {c.ok ? <CheckIcon className="mt-0.5 size-4 shrink-0 text-success" aria-hidden="true" /> : <XIcon className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden="true" />}
            <span>
              <span className="font-medium">{c.name}</span>
              <span className="sr-only">{c.ok ? " passed" : " failed"}</span>: <span className="text-muted-foreground">{c.detail}</span>
              {!c.ok && !c.detail?.startsWith("not checked") && checkHelp(c.name, c.detail, kind) ? <span className="block">{checkHelp(c.name, c.detail, kind)}</span> : null}
            </span>
          </li>
        ))}
      </ul>
      {children}
    </div>
  )
}

/** A certificate failure offers the company's certificate, which goes in the connection's ca_bundle as a plan. */
function AddCertificate({ connection }: { connection: TrackerConnection }) {
  const [pem, setPem] = useState("")
  const [error, setError] = useState("")
  const draft = useDraft()
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!/-----BEGIN CERTIFICATE-----/.test(pem)) {
      setError("Paste the certificate of your company's authority: it starts with -----BEGIN CERTIFICATE-----.")
      return
    }
    setError("")
    const [current] = await api.getResources("tracker_connection", connection.name)
    const spec = (current?.spec ?? {}) as { config?: Record<string, unknown> }
    await draft.make(`Trust the certificate of ${connection.name}`, [
      { kind: "tracker_connection", name: connection.name, spec: { ...spec, config: { ...spec.config, ca_bundle: pem.trim() } } },
    ])
  }
  if (draft.plan) return <DraftPlan draft={draft} onEdit={() => void draft.discard()} onDone={() => void draft.discard()} />
  return (
    <form onSubmit={(e) => void submit(e)} aria-label="Add the company's certificate" className="space-y-2">
      <label htmlFor={`pem-${connection.name}`} className="text-sm font-medium">
        Your company's certificate (PEM)
      </label>
      <Textarea id={`pem-${connection.name}`} rows={5} className="font-mono text-xs" value={pem} onChange={(e) => setPem(e.target.value)} aria-invalid={Boolean(error)} />
      <p className="text-xs text-muted-foreground">Only certificates are accepted. Verification is never turned off.</p>
      {error || draft.error ? (
        <p role="alert" className="text-sm text-destructive">
          {error || draft.error}
        </p>
      ) : null}
      <PermButton permission="trackers.manage" type="submit" variant="outline" disabled={draft.busy}>
        Add the certificate
      </PermButton>
    </form>
  )
}

function Rule({ r, onRemove }: { r: TrackerRoute; onRemove: () => void }) {
  const states = Object.entries(r.states ?? {})
  return (
    <li className="flex flex-wrap items-start justify-between gap-2 rounded-lg border bg-card p-3 text-sm">
      <div className="min-w-0 space-y-1">
        <p>{routeSentence(r)}</p>
        {states.length > 0 ? <p className="text-xs text-muted-foreground">Status: {states.map(([k, v]) => `${k.replace("_", " ")} → ${v}`).join(", ")}</p> : null}
        {r.warning ? (
          <p className="flex items-start gap-1.5 text-warning">
            <TriangleAlertIcon className="mt-0.5 size-4 shrink-0" aria-hidden="true" />
            <span>
              <span className="sr-only">Warning: </span>
              {r.warning}
            </span>
          </p>
        ) : null}
      </div>
      <PermButton permission="trackers.manage" variant="outline" aria-label={`Remove the rule ${r.connection}/${r.position}`} onClick={onRemove}>
        Remove
      </PermButton>
    </li>
  )
}

/**
 * A connected tracker: how it was tested, where its tickets go, who is not matched yet and what waits.
 * The lists come from the page; changes are plans.
 */
export function TrackerCard({
  connection, routes, people, tickets, test, testing, testError, onTest,
}: {
  connection: TrackerConnection
  routes: TrackerRoute[]
  people: UnmappedPerson[]
  tickets: TicketInfo[]
  test?: { result: TrackerTest; at: string }
  testing: boolean
  testError?: string
  onTest: () => void
}) {
  const [adding, setAdding] = useState(false)
  const [showPeople, setShowPeople] = useState(false)
  const [showTickets, setShowTickets] = useState(false)
  const remove = useDraft()
  const mine = routes.filter((r) => r.connection === connection.name).sort((a, b) => a.position - b.position)
  const queued = tickets.filter((t) => t.status === "queued")
  const limit = queued.find((t) => t.limit)?.limit
  const certFailed = test?.result.checks.some((c) => c.name === "certificate" && !c.ok)

  return (
    <section aria-label={`${kindLabel(connection.kind)} ${connection.name}`} className="space-y-4 rounded-xl border bg-card p-4">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h3 className="text-base font-semibold">
            {kindLabel(connection.kind)} · {addressOf(connection)}
          </h3>
          <p className="text-sm text-muted-foreground">
            <span className="font-mono">{connection.name}</span>: tickets {triggerWords(connection.triggers)}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {test ? (
            <StatusBadge tone={test.result.ok ? "good" : "bad"}>
              {test.result.ok ? "Works" : "Needs attention"} · checked <Age iso={test.at} /> ago
            </StatusBadge>
          ) : (
            <StatusBadge>Not tested here yet</StatusBadge>
          )}
          <PermButton permission="trackers.manage" variant="outline" aria-label={`Test connection of ${connection.name}`} disabled={testing} onClick={onTest}>
            {testing ? "Testing…" : "Test connection"}
          </PermButton>
        </div>
      </div>
      {testError ? (
        <p role="alert" className="text-sm text-destructive">
          {testError}
        </p>
      ) : null}
      {test ? <Results test={test.result} kind={connection.kind}>{certFailed && connection.kind === "jira" ? <AddCertificate connection={connection} /> : null}</Results> : null}

      <div className="space-y-2">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h4 className="text-sm font-medium">Tickets go to</h4>
          <PermButton permission="trackers.manage" variant="outline" aria-label={`Add a rule to ${connection.name}`} onClick={() => setAdding(true)}>
            Add a rule
          </PermButton>
        </div>
        {remove.plan ? <DraftPlan draft={remove} onEdit={() => void remove.discard()} onDone={() => void remove.discard()} /> : null}
        {mine.length === 0 ? (
          <p className="text-sm text-muted-foreground">No rule yet: tickets handed over wait in the Inbox until one says where they go.</p>
        ) : (
          <ul aria-label={`Rules of ${connection.name}`} className="space-y-2">
            {mine.map((r) => (
              <Rule key={r.id} r={r} onRemove={() => void remove.make(`Remove the rule ${r.connection}/${r.position}`, [{ kind: "tracker_route", name: `${r.connection}/${r.position}`, delete: true }])} />
            ))}
          </ul>
        )}
      </div>

      <div className="space-y-2 text-sm">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p>
            <span className="font-medium">People</span>{" "}
            <span className="text-muted-foreground">{people.length === 0 ? "everyone who handed over a ticket is matched" : `${people.length} ${people.length === 1 ? "person" : "people"} not matched yet`}</span>
          </p>
          {people.length > 0 ? (
            <Button variant="outline" aria-expanded={showPeople} aria-label={`Review the people of ${connection.name}`} onClick={() => setShowPeople(!showPeople)}>
              Review
            </Button>
          ) : null}
        </div>
        {showPeople && people.length > 0 ? <PeopleReview people={people} /> : null}
        <div className="flex flex-wrap items-center justify-between gap-2">
          <p>
            <span className="font-medium">Waiting</span>{" "}
            <span className="text-muted-foreground">
              {tickets.length === 0 ? "no tickets" : `${tickets.length} ${tickets.length === 1 ? "ticket" : "tickets"}${limit && queued.length > 0 ? `, limit of ${limit} goals running` : ""}`}
            </span>
          </p>
          {tickets.length > 0 ? (
            <Button variant="outline" aria-expanded={showTickets} aria-label={`Open the waiting tickets of ${connection.name}`} onClick={() => setShowTickets(!showTickets)}>
              Open
            </Button>
          ) : null}
        </div>
        {showTickets && tickets.length > 0 ? <WaitingTickets tickets={tickets} /> : null}
      </div>
      <AddRule open={adding} onOpenChange={setAdding} connection={connection} routes={routes} states={test?.result.states} />
    </section>
  )
}
