import { Link, useNavigate, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { useDeleteNote, useGoals, useKnowledge, useKnowledgeProposals, useKnowledgeSections, useProject, useProposeKnowledge, useTrackerConnections } from "@/api/queries"
import type { KnowledgeAction, KnowledgeProposal, KnowledgeProposalStatus, ProposeKnowledgeResult } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { Capped } from "@/components/Capped"
import { ConfirmDialog } from "@/components/ConfirmDialog"
import { ExternalLink } from "@/components/ExternalLink"
import { Field } from "@/components/Field"
import { Markdown } from "@/components/Markdown"
import { StatusBadge } from "@/components/StatusBadge"
import { useCan } from "@/lib/permissions"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Textarea } from "@/components/ui/textarea"
import { goalRef, type Tone } from "@/lib/format"

const STATUSES: KnowledgeProposalStatus[] = ["queued", "open", "accepted", "rejected"]
const proposalTone = (s: KnowledgeProposalStatus): Tone => (s === "accepted" ? "good" : s === "rejected" ? "bad" : s === "open" ? "attention" : "neutral")

/** Where a proposal is reviewed: its pull request, or the gate a human decides. */
function Review({ p }: { p: KnowledgeProposal }) {
  if (p.url) return <ExternalLink href={p.url}>Open the pull request</ExternalLink>
  if (!p.gate) return <span className="text-muted-foreground">no review yet</span>
  if (p.gate.status !== "pending") return <span>{p.gate.ref}: {p.gate.status.replaceAll("_", " ")}</span>
  return (
    <Link to="/inbox" search={{ gate: p.gate.ref }} className="text-primary underline underline-offset-2">
      Decide {p.gate.ref}
    </Link>
  )
}

function ProposalItem({ p }: { p: KnowledgeProposal }) {
  return (
    <article aria-labelledby={`${p.ref}-path`} className="space-y-2 rounded-xl border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-mono">{p.ref}</span>
        <StatusBadge tone={proposalTone(p.status)}>{p.status}</StatusBadge>
        <StatusBadge>{p.action}</StatusBadge>
        <span className="ml-auto text-muted-foreground">
          <Age iso={p.created_at} ago />
        </span>
      </div>
      <h3 id={`${p.ref}-path`} className="font-mono text-sm font-medium break-all">
        {p.section} / {p.path}
      </h3>
      <p className="text-sm break-words whitespace-pre-wrap">{p.reason}</p>
      <p className="text-sm text-muted-foreground">
        Proposed by {p.proposed_by}
        {p.goal_ref ? (
          <>
            {" "}
            for{" "}
            <Link to="/goals/$ref" params={{ ref: p.goal_ref }} className="font-mono underline underline-offset-2">
              {p.goal_ref}
            </Link>
          </>
        ) : null}
        . <Review p={p} />
      </p>
      {p.body ? (
        <details className="text-sm">
          <summary className="cursor-pointer py-1 font-medium">New text of {p.path}</summary>
          <Markdown>{p.body}</Markdown>
        </details>
      ) : null}
    </article>
  )
}

function ProposeForm({ project }: { project: string }) {
  const sections = useKnowledgeSections(project)
  const propose = useProposeKnowledge()
  const [section, setSection] = useState("")
  const [path, setPath] = useState("")
  const [action, setAction] = useState<KnowledgeAction | "">("")
  const [body, setBody] = useState("")
  const [reason, setReason] = useState("")
  const [errors, setErrors] = useState<{ section?: string; path?: string; body?: string; reason?: string }>({})
  const [done, setDone] = useState<ProposeKnowledgeResult | null>(null)
  const curated = sections.data?.filter((s) => s.provider && s.provider !== "internal") ?? []

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    const next: typeof errors = {}
    if (!section) next.section = "Choose a section."
    if (!path.trim()) next.path = "Give the path of the entry."
    if (action !== "retire" && !body.trim()) next.body = "Write the entry, or choose retire."
    if (!reason.trim()) next.reason = "Say why the knowledge should change."
    setErrors(next)
    if (Object.keys(next).length) return
    setDone(null)
    propose.mutate(
      { project, section, path: path.trim(), action: action || undefined, body: action === "retire" ? "" : body, reason: reason.trim() },
      {
        onSuccess: (r) => {
          setDone(r)
          setPath("")
          setBody("")
          setReason("")
        },
      },
    )
  }

  return (
    <form onSubmit={submit} noValidate aria-labelledby="kb-propose" className="space-y-4 rounded-xl border bg-card p-4">
      <h2 id="kb-propose" className="text-lg font-semibold">
        Propose a change
      </h2>
      <p className="text-sm text-muted-foreground">It goes straight to a pull request. In a protected section you are recorded as the approver.</p>
      <div className="grid gap-4 sm:grid-cols-2">
        <Field label="Section" error={errors.section}>
          {(p) => (
            <NativeSelect {...p} className="w-full" value={section} onChange={(e) => setSection(e.target.value)}>
              <NativeSelectOption value="">Choose a section</NativeSelectOption>
              {curated.map((s) => (
                <NativeSelectOption key={s.name} value={s.name}>
                  {s.name}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </Field>
        <Field label="Action" hint="Automatic edits an entry that exists and adds one that does not.">
          {(p) => (
            <NativeSelect {...p} className="w-full" value={action} onChange={(e) => setAction(e.target.value as KnowledgeAction | "")}>
              <NativeSelectOption value="">Automatic</NativeSelectOption>
              <NativeSelectOption value="add">add</NativeSelectOption>
              <NativeSelectOption value="edit">edit</NativeSelectOption>
              <NativeSelectOption value="retire">retire</NativeSelectOption>
            </NativeSelect>
          )}
        </Field>
      </div>
      <Field label="Path" hint="The repo path of the entry, inside the section." error={errors.path}>
        {(p) => <Input {...p} value={path} onChange={(e) => setPath(e.target.value)} className="font-mono" />}
      </Field>
      {action === "retire" ? null : (
        <Field label="Entry (Markdown)" hint="The full new text, without frontmatter." error={errors.body}>
          {(p) => <Textarea {...p} rows={8} value={body} onChange={(e) => setBody(e.target.value)} />}
        </Field>
      )}
      <Field label="Reason" error={errors.reason}>
        {(p) => <Textarea {...p} rows={2} value={reason} onChange={(e) => setReason(e.target.value)} />}
      </Field>
      {propose.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {propose.error.message}
        </p>
      ) : null}
      {done ? (
        <p role="status" className="text-sm">
          {done.proposal} opened as <ExternalLink href={done.url}>pull request #{done.number}</ExternalLink>.
        </p>
      ) : null}
      <Button type="submit" disabled={propose.isPending}>
        Propose
      </Button>
    </form>
  )
}

export function ProposalsTab({ project }: { project: string }) {
  const [status, setStatus] = useState<KnowledgeProposalStatus | "">("")
  const proposals = useKnowledgeProposals(project, status)
  return (
    <div className="space-y-6">
      <section aria-labelledby="kb-proposals" className="space-y-3">
        <div className="flex flex-wrap items-end justify-between gap-3">
          <h2 id="kb-proposals" className="text-lg font-semibold">
            Proposals
          </h2>
          <div className="space-y-1.5">
            <Label htmlFor="kb-status">Status</Label>
            <NativeSelect id="kb-status" className="w-full sm:w-44" value={status} onChange={(e) => setStatus(e.target.value as KnowledgeProposalStatus | "")}>
              <NativeSelectOption value="">All statuses</NativeSelectOption>
              {STATUSES.map((s) => (
                <NativeSelectOption key={s} value={s}>
                  {s}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </div>
        </div>
        <Async query={proposals} isEmpty={(d) => d.length === 0} empty="No proposals.">
          {(items) => (
            <>
              <ul aria-label="Knowledge proposals" className="space-y-3">
                {items.map((p) => (
                  <li key={p.ref}>
                    <ProposalItem p={p} />
                  </li>
                ))}
              </ul>
              <Capped n={items.length} />
            </>
          )}
        </Async>
      </section>
      <ProposeForm project={project} />
    </div>
  )
}

/** The notes agents wrote: the project's, or the project's and one goal's. */
export function NotesTab({ project }: { project: string }) {
  const { goal = "" } = useSearch({ from: "/app/knowledge" })
  const navigate = useNavigate({ from: "/knowledge" })
  const goals = useGoals(project)
  const notes = useKnowledge(project, { kind: "note", goal })
  const remove = useDeleteNote()
  return (
    <section aria-labelledby="kb-notes" className="space-y-3">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2 id="kb-notes" className="text-lg font-semibold">
            Agent notes
          </h2>
          <p className="text-sm text-muted-foreground">What agents wrote down for later jobs. Delete a note that is wrong or no longer matters.</p>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="kb-goal">Notes of</Label>
          <NativeSelect
            id="kb-goal"
            className="w-full sm:w-64"
            value={goal}
            onChange={(e) => void navigate({ search: (prev) => ({ ...prev, goal: e.target.value || undefined }) })}
          >
            <NativeSelectOption value="">The project</NativeSelectOption>
            {goals.data?.map((g) => (
              <NativeSelectOption key={g.id} value={goalRef(g.number)}>
                {goalRef(g.number)} · {g.title} (and the project's)
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
      </div>
      <Async query={notes} isEmpty={(d) => d.entries.length === 0} empty="No notes.">
        {(d) => (
          <>
          <ul aria-label="Agent notes" className="space-y-3">
            {d.entries.map((n) => (
              <li key={n.id}>
                <article aria-labelledby={`note-${n.id}`} className="space-y-2 rounded-xl border bg-card p-4">
                  <div className="flex flex-wrap items-center gap-2 text-sm">
                    <StatusBadge>{n.scope === "goal" ? "goal note" : "project note"}</StatusBadge>
                    {n.tags?.map((t) => (
                      <StatusBadge key={t}>{t}</StatusBadge>
                    ))}
                    <span className="text-muted-foreground">
                      by {n.provenance.author_role}
                      {n.provenance.source ? ` (${n.provenance.source})` : ""}, <Age iso={n.provenance.created} ago />
                    </span>
                    {n.expires_at ? (
                      <span className="text-muted-foreground">
                        expires <Age iso={n.expires_at} future />
                      </span>
                    ) : null}
                    <span className="ml-auto">
                      <ConfirmDialog
                        trigger="Delete"
                        triggerAriaLabel={`Delete note ${n.title}`}
                        title="Delete this note?"
                        description="Later jobs will not see it. It cannot be restored."
                        confirmLabel="Delete note"
                        keepLabel="Keep note"
                        mutation={remove}
                        variables={n.id}
                      />
                    </span>
                  </div>
                  <h3 id={`note-${n.id}`} className="font-medium break-words">
                    {n.title}
                  </h3>
                  <Markdown>{n.body}</Markdown>
                </article>
              </li>
            ))}
          </ul>
          <Capped n={d.entries.length} />
          </>
        )}
      </Async>
    </section>
  )
}

/** A Jira or Linear the repo mentions and no connection serves: the tracker screens set it up. */
function ConnectLink({ tool, instance }: { tool: string; instance?: string }) {
  const can = useCan()
  const connections = useTrackerConnections(can("trackers.read"))
  const kind = tool.toLowerCase()
  if ((kind !== "jira" && kind !== "linear") || !connections.data || connections.data.some((c) => c.kind === kind)) return null
  if (!can("trackers.manage")) {
    return (
      <p className="text-muted-foreground">
        Connecting {tool} as a tracker needs <code className="font-mono">trackers.manage</code>.
      </p>
    )
  }
  return (
    <p>
      <Link to="/settings/integrations" search={{ connect: kind, ...(instance ? { instance } : {}) }} className="text-primary underline underline-offset-2">
        Connect {tool} as a tracker
      </Link>
    </p>
  )
}

/** The tools the team uses outside the repo, as the approved profile lists them. */
export function IntegrationsTab({ project }: { project: string }) {
  const p = useProject(project)
  return (
    <section aria-labelledby="kb-integrations" className="space-y-3">
      <h2 id="kb-integrations" className="text-lg font-semibold">
        Integrations
      </h2>
      <Async query={p} isEmpty={(d) => !d.profile?.integrations?.length} empty="Onboarding found no tools outside the repo.">
        {(d) => (
          <ul aria-label="Integrations" className="space-y-3">
            {d.profile!.integrations!.map((i) => (
              <li key={`${i.tool}-${i.instance ?? ""}`} className="space-y-1 rounded-xl border bg-card p-4 text-sm">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium">{i.tool}</span>
                  {i.instance ? <span className="font-mono text-muted-foreground">{i.instance}</span> : null}
                  <StatusBadge tone={i.status === "connected" ? "good" : i.status === "approved" ? "neutral" : "attention"}>{i.status}</StatusBadge>
                  <span className="text-muted-foreground">{i.confidence} confidence</span>
                </div>
                <p>Could be used for {i.uses.join(", ")}.</p>
                {i.status !== "connected" ? <ConnectLink tool={i.tool} instance={i.instance} /> : null}
                {i.mapping
                  ? Object.entries(i.mapping).map(([section, locations]) => (
                      <p key={section} className="text-muted-foreground">
                        <span className="font-medium">{section}</span>: <span className="font-mono break-all">{locations.join(", ")}</span>
                      </p>
                    ))
                  : null}
              </li>
            ))}
          </ul>
        )}
      </Async>
    </section>
  )
}
