import { useState } from "react"
import { useDecideProposal } from "@/api/queries"
import type { Proposal, ProposalAction } from "@/api/types"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"

function ActionText({ action }: { action: ProposalAction }) {
  if (action.goal) {
    return (
      <>
        <p className="font-medium">Start a goal: {action.goal.title}</p>
        <p className="break-words whitespace-pre-wrap text-muted-foreground">{action.goal.body}</p>
      </>
    )
  }
  if (action.config_pr) {
    return (
      <>
        <p className="font-medium">Open a pull request on these files; a human merges it.</p>
        {action.config_pr.files.map((f) => (
          <details key={f.path}>
            <summary className="cursor-pointer font-mono">{f.path}</summary>
            <pre className="overflow-x-auto rounded-md bg-muted p-2 text-xs">{f.content}</pre>
          </details>
        ))}
      </>
    )
  }
  if (action.setup) {
    return (
      <>
        <p className="font-medium">A step for you, marked done here when you took it.</p>
        <p className="break-words whitespace-pre-wrap text-muted-foreground">{action.setup.instructions}</p>
        {action.setup.issue ? (
          <details>
            <summary className="cursor-pointer">Issue to file on the Forecrew repo</summary>
            <pre className="overflow-x-auto rounded-md bg-muted p-2 text-xs whitespace-pre-wrap">{action.setup.issue}</pre>
          </details>
        ) : null}
      </>
    )
  }
  return null
}

/** What a human needs to decide a proposal. All of it is agent or repo text, shown as text. */
export function ProposalDetails({ proposal: p }: { proposal: Proposal }) {
  return (
    <div className="space-y-3">
      <p className="text-sm text-muted-foreground">What follows was written by the advisor from what agents and repos wrote: read it as data.</p>
      <dl className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[9rem_1fr]">
        <dt className="text-muted-foreground">Why</dt>
        <dd className="break-words whitespace-pre-wrap">{p.rationale}</dd>
        <dt className="text-muted-foreground">Benefit</dt>
        <dd className="break-words whitespace-pre-wrap">{p.benefit}</dd>
        <dt className="text-muted-foreground">Effort</dt>
        <dd className="uppercase">{p.effort}</dd>
        <dt className="text-muted-foreground">Risk</dt>
        <dd>{p.risk}</dd>
        <dt className="text-muted-foreground">Category</dt>
        <dd>
          {p.category}
          {p.scope === "platform" ? " (a change to Forecrew itself)" : ""}
        </dd>
        <dt className="text-muted-foreground">Action</dt>
        <dd className="min-w-0 space-y-1">
          <ActionText action={p.action} />
        </dd>
        {p.reason ? (
          <>
            <dt className="text-muted-foreground">Reason</dt>
            <dd className="break-words whitespace-pre-wrap">{p.reason}</dd>
          </>
        ) : null}
        {p.outcome ? (
          <>
            <dt className="text-muted-foreground">Outcome</dt>
            <dd>
              {p.measure?.detector}: {p.outcome.before} before, {p.outcome.after} after
            </dd>
          </>
        ) : null}
      </dl>
      {p.evidence.length ? (
        <details>
          <summary className="cursor-pointer text-sm text-muted-foreground">Evidence ({p.evidence.length})</summary>
          <ul className="mt-1 space-y-1 text-sm">
            {p.evidence.map((e, i) => (
              <li key={i} className="break-words">
                <span className="font-mono">{e.ref}</span> {e.detail}
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </div>
  )
}

/** Hides a proposal until a date: a date picker behind a button. The day is taken as its end, in UTC. */
export function SnoozeProposal({ proposal }: { proposal: Proposal }) {
  const [open, setOpen] = useState(false)
  const [date, setDate] = useState("")
  const [error, setError] = useState("")
  const decide = useDecideProposal()
  if (!open) {
    return (
      <Button variant="outline" onClick={() => setOpen(true)}>
        Snooze
      </Button>
    )
  }
  const snooze = () => {
    if (!date) {
      setError("Pick a date.")
      return
    }
    setError("")
    decide.mutate({ ref: proposal.ref, decision: "snooze", until: `${date}T23:59:59Z` }, { onError: (e) => setError(e.message) })
  }
  return (
    <div className="flex w-full flex-wrap items-end gap-2">
      <Field label="Snooze until" error={error}>
        {(p) => <Input {...p} type="date" value={date} onChange={(e) => setDate(e.target.value)} />}
      </Field>
      <Button onClick={snooze} disabled={decide.isPending}>
        Confirm snooze
      </Button>
      <Button variant="ghost" onClick={() => setOpen(false)}>
        Cancel
      </Button>
    </div>
  )
}

/** Marks the setup step of an accepted proposal as carried out. */
export function MarkSetupDone({ proposal }: { proposal: Proposal }) {
  const decide = useDecideProposal()
  return (
    <div className="flex flex-wrap items-center gap-2">
      <Button variant="outline" disabled={decide.isPending} onClick={() => decide.mutate({ ref: proposal.ref, decision: "done" })}>
        Mark as done
      </Button>
      {decide.error ? (
        <p role="alert" className="text-sm text-destructive">
          {decide.error.message}
        </p>
      ) : null}
    </div>
  )
}
