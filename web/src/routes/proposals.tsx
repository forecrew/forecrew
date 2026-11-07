import { Link, useNavigate, useSearch } from "@tanstack/react-router"
import { useProposals } from "@/api/queries"
import { PROPOSAL_CATEGORIES, PROPOSAL_STATUSES, type Proposal, type ProposalCategory, type ProposalStatus } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { PageHeader } from "@/components/PageHeader"
import { MarkSetupDone, ProposalDetails } from "@/components/ProposalDetails"
import { StatusBadge } from "@/components/StatusBadge"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { proposalTone } from "@/lib/format"

function Item({ p }: { p: Proposal }) {
  return (
    <article aria-labelledby={`${p.ref}-title`} className="space-y-2 rounded-xl border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm">{p.ref}</span>
        <StatusBadge tone={proposalTone(p.status)}>{p.status}</StatusBadge>
        {p.goal_ref ? <span className="font-mono text-sm text-muted-foreground">{p.goal_ref}</span> : null}
        <span className="ml-auto flex items-center gap-3 text-sm text-muted-foreground">
          <Age iso={p.created_at} ago />
          {p.status === "proposed" && p.gate_ref ? (
            <Link to="/inbox" search={{ gate: p.gate_ref } as never} className="text-primary underline underline-offset-2">
              Decide in the Inbox
            </Link>
          ) : null}
        </span>
      </div>
      <h2 id={`${p.ref}-title`} className="font-medium break-words">
        {p.title}
      </h2>
      <ProposalDetails proposal={p} />
      {p.status === "accepted" && p.action.setup && p.can_decide ? <MarkSetupDone proposal={p} /> : null}
    </article>
  )
}

export function ProposalsPage() {
  const { project = "", status = "", category = "" } = useSearch({ from: "/app/proposals" })
  const navigate = useNavigate({ from: "/proposals" })
  const proposals = useProposals(project, status as ProposalStatus | "", category as ProposalCategory | "")
  const setFilter = (next: { status?: string; category?: string }) =>
    void navigate({ search: (prev) => ({ ...prev, ...Object.fromEntries(Object.entries(next).map(([k, v]) => [k, v || undefined])) }) })

  return (
    <>
      <PageHeader title="Proposals" meta="What Forecrew suggests a project should gain, what you decided, and how it turned out." />
      <div className="mb-4 flex flex-wrap gap-4">
        <div className="space-y-1.5">
          <Label htmlFor="proposal-status">Status</Label>
          <NativeSelect id="proposal-status" className="w-full sm:w-48" value={status} onChange={(e) => setFilter({ status: e.target.value })}>
            <NativeSelectOption value="">All statuses</NativeSelectOption>
            {PROPOSAL_STATUSES.map((s) => (
              <NativeSelectOption key={s} value={s}>
                {s}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="proposal-category">Category</Label>
          <NativeSelect id="proposal-category" className="w-full sm:w-48" value={category} onChange={(e) => setFilter({ category: e.target.value })}>
            <NativeSelectOption value="">Any category</NativeSelectOption>
            {PROPOSAL_CATEGORIES.map((c) => (
              <NativeSelectOption key={c} value={c}>
                {c}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
      </div>
      <Async query={proposals} isEmpty={(d) => d.length === 0} empty="No proposals match.">
        {(items) => (
          <ul aria-label="Proposals" className="space-y-3">
            {items.map((p) => (
              <li key={p.id}>
                <Item p={p} />
              </li>
            ))}
          </ul>
        )}
      </Async>
    </>
  )
}
