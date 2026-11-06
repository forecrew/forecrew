import { Link, useParams } from "@tanstack/react-router"
import { useState } from "react"
import { useTrace } from "@/api/queries"
import type { Trace, TraceKind, TraceNode } from "@/api/types"
import { Age } from "@/components/Age"
import { AssumptionResolve } from "@/components/AssumptionResolve"
import { Async } from "@/components/Async"
import { ExternalLink } from "@/components/ExternalLink"
import { Markdown } from "@/components/Markdown"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { words, type Tone } from "@/lib/format"
import { cn } from "cn"

const KIND: Record<TraceKind, string> = {
  goal: "Goal",
  council: "Council",
  spec: "Spec",
  decision: "Decision",
  task: "Task",
  change: "Pull request",
  verification: "Verification",
  merge: "Merge",
  assumption: "Assumption",
}

const GOOD = ["merged", "passed", "approved", "verified", "decided", "done", "accepted", "success"]
const BAD = ["failed", "invalidated", "rejected", "canceled", "closed"]
const ATTENTION = ["unverified", "incomplete", "in_review", "merge_ready", "spec_review", "open", "running"]

/** The tone of a status of any node kind. A decision's status is its choice, which stays neutral. */
export function traceTone(n: TraceNode): Tone {
  const s = n.status ?? ""
  if (n.kind === "decision") return "neutral"
  return GOOD.includes(s) ? "good" : BAD.includes(s) ? "bad" : ATTENTION.includes(s) ? "attention" : "neutral"
}

/** Verification reports and merges have no ref of their own: they cannot start a trace. */
const hasRef = (n: TraceNode) => n.kind !== "verification" && n.kind !== "merge"

/** Where each node came from: the nodes with an edge to it, except assumptions. */
function parents(t: Trace, n: TraceNode) {
  const byId = new Map(t.nodes.map((x) => [x.id, x]))
  return t.edges.filter((e) => e.to === n.id && byId.get(e.from)?.kind !== "assumption").map((e) => ({ id: e.from, label: e.label }))
}

function Attrs({ node }: { node: TraceNode }) {
  const rows = Object.entries(node.attrs ?? {}).filter(([, v]) => v)
  if (rows.length === 0) return null
  return (
    <dl className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[8rem_1fr]">
      {rows.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-muted-foreground">{words(k)}</dt>
          <dd className="break-words whitespace-pre-wrap">{v}</dd>
        </div>
      ))}
    </dl>
  )
}

function Detail({ node, trace }: { node: TraceNode; trace: Trace }) {
  const ref = node.id
  return (
    <section aria-labelledby="trace-detail" className="space-y-3 rounded-xl border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm text-muted-foreground">{KIND[node.kind]}</span>
        <span className="font-mono text-sm">{ref}</span>
        {node.status ? <StatusBadge tone={traceTone(node)}>{words(node.status)}</StatusBadge> : null}
        <span className="text-sm text-muted-foreground">
          <Age iso={node.at} ago />
        </span>
      </div>
      <h2 id="trace-detail" className="text-lg font-semibold break-words">
        {node.title}
      </h2>
      {node.summary ? <Markdown>{node.summary}</Markdown> : null}
      <Attrs node={node} />
      <div className="flex flex-wrap items-center gap-3 text-sm">
        {node.url ? <ExternalLink href={node.url}>Open on GitHub</ExternalLink> : null}
        {trace.goal && ["goal", "spec", "task"].includes(node.kind) ? (
          <Link to="/goals/$ref" params={{ ref: trace.goal }} className="text-primary underline underline-offset-2">
            Open {trace.goal}
          </Link>
        ) : null}
        {hasRef(node) && ref !== trace.focus ? (
          <Link to="/trace/$ref" params={{ ref }} className="text-primary underline underline-offset-2">
            Trace {ref}
          </Link>
        ) : null}
      </div>
      {node.kind === "assumption" && node.status ? <AssumptionResolve assumption={{ ref, status: node.status as never }} /> : null}
    </section>
  )
}

function Chain({ trace, selected, onSelect }: { trace: Trace; selected: string; onSelect: (id: string) => void }) {
  const byId = new Map(trace.nodes.map((n) => [n.id, n]))
  const attached = (id: string) => trace.edges.filter((e) => e.to === id && byId.get(e.from)?.kind === "assumption").map((e) => byId.get(e.from)!)
  const loose = trace.nodes.filter((n) => n.kind === "assumption" && !trace.edges.some((e) => e.from === n.id))
  const steps = trace.nodes.filter((n) => n.kind !== "assumption")
  const pick = (n: TraceNode, extra?: string) => (
    <button
      type="button"
      aria-current={n.id === selected ? "true" : undefined}
      onClick={() => onSelect(n.id)}
      className={cn("block w-full border-l-2 border-transparent px-2 py-1 text-left text-sm hover:bg-accent", n.id === selected && "border-primary font-medium", extra)}
    >
      <span className="flex flex-wrap items-center gap-2">
        <span className="text-xs text-muted-foreground">{KIND[n.kind]}</span>
        <span className="font-mono text-xs">{n.id}</span>
        {n.status ? <StatusBadge tone={traceTone(n)}>{words(n.status)}</StatusBadge> : null}
      </span>
      <span className="block break-words">{n.title}</span>
    </button>
  )
  return (
    <ol aria-label="Trace" className="space-y-3 border-l pl-4">
      {steps.map((n) => (
        <li key={n.id} className="space-y-1">
          {pick(n)}
          {parents(trace, n).length ? (
            <p className="px-2 text-xs text-muted-foreground">{parents(trace, n).map((p) => `${p.label ?? "from"} ${p.id.replace(/^(verification|merge):/, "")}`).join(", ")}</p>
          ) : null}
          {attached(n.id).length ? (
            <ul aria-label={`Assumptions of ${n.id}`} className="ml-4 space-y-1 border-l pl-2">
              {attached(n.id).map((a) => (
                <li key={a.id}>{pick(a)}</li>
              ))}
            </ul>
          ) : null}
        </li>
      ))}
      {loose.map((a) => (
        <li key={a.id}>{pick(a)}</li>
      ))}
    </ol>
  )
}

export function TracePage() {
  const { ref } = useParams({ from: "/app/trace/$ref" })
  const trace = useTrace(ref)
  const [picked, setPicked] = useState<{ ref: string; id: string } | null>(null)
  return (
    <Async query={trace} isEmpty={(t) => t.nodes.length === 0} empty="Nothing to trace.">
      {(t) => {
        // A choice belongs to the trace it was made in: following a link starts from its focus.
        const id = picked && picked.ref === ref && t.nodes.some((n) => n.id === picked.id) ? picked.id : t.focus
        const node = t.nodes.find((n) => n.id === id) ?? t.nodes[0]!
        return (
          <>
            <PageHeader
              title={`Why ${ref}?`}
              meta={
                t.goal ? (
                  <>
                    The chain behind it, from goal{" "}
                    <Link to="/goals/$ref" params={{ ref: t.goal }} className="font-mono underline underline-offset-2">
                      {t.goal}
                    </Link>{" "}
                    to the merged change.
                  </>
                ) : (
                  "This record belongs to no goal."
                )
              }
            />
            <div className="grid grid-cols-1 items-start gap-6 lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
              <Chain trace={t} selected={node.id} onSelect={(i) => setPicked({ ref, id: i })} />
              <Detail node={node} trace={t} />
            </div>
          </>
        )
      }}
    </Async>
  )
}
