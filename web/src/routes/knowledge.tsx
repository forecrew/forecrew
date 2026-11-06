import { Link, useNavigate, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { useKnowledge, useKnowledgeSearch, useProjects } from "@/api/queries"
import type { KnowledgeKind, KnowledgeResult, KnowledgeSection } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { ExternalLink } from "@/components/ExternalLink"
import { Markdown } from "@/components/Markdown"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { IntegrationsTab, NotesTab, ProposalsTab } from "@/routes/knowledge-tabs"
import { cn } from "cn"

const TABS = [
  { id: "entries", label: "Entries" },
  { id: "proposals", label: "Proposals" },
  { id: "notes", label: "Agent notes" },
  { id: "integrations", label: "Integrations" },
] as const
type Tab = (typeof TABS)[number]["id"]

const KINDS: KnowledgeKind[] = ["entry", "note", "generated"]

/** An entry as the list gives it (with its body), or a search hit (with an excerpt). */
type Item = KnowledgeResult & { body?: string }

function Sections({ sections, section, onPick }: { sections: KnowledgeSection[]; section: string; onPick: (name: string) => void }) {
  return (
    <section aria-labelledby="kb-sections" className="space-y-2">
      <h2 id="kb-sections" className="text-lg font-semibold">
        Sections
      </h2>
      <ul className="overflow-hidden rounded-xl border bg-card">
        {sections.map((s) => (
          <li key={s.name} className="border-b last:border-b-0">
            <button
              type="button"
              aria-pressed={s.name === section}
              onClick={() => onPick(s.name === section ? "" : s.name)}
              className={cn("block w-full space-y-0.5 border-l-2 border-transparent px-3 py-2 text-left text-sm hover:bg-accent", s.name === section && "border-primary")}
            >
              <span className="flex flex-wrap items-center gap-2 font-medium">
                {s.name}
                {s.protected ? <StatusBadge tone="attention">protected</StatusBadge> : null}
              </span>
              <span className="block text-muted-foreground">
                {s.provider ? (
                  <>
                    <span className="font-mono">{s.provider}</span>
                    {s.provider_kind && s.provider_kind !== s.provider ? ` (${s.provider_kind}${s.instance ? `, ${s.instance}` : ""})` : ""}
                  </>
                ) : (
                  "built by forecrewd"
                )}
                {s.owner ? ` · owner ${s.owner}` : ""}
              </span>
              {s.paths.length ? <span className="block font-mono text-xs break-all text-muted-foreground">{s.paths.join(", ")}</span> : null}
            </button>
          </li>
        ))}
      </ul>
    </section>
  )
}

function EntryView({ item }: { item: Item }) {
  const p = item.provenance
  return (
    <section aria-labelledby="kb-entry" className="space-y-3 rounded-xl border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <StatusBadge>{item.kind}</StatusBadge>
        {item.untrusted ? <StatusBadge tone="attention">untrusted text</StatusBadge> : null}
        {item.needs_reverify ? <StatusBadge tone="attention">needs re-verify</StatusBadge> : null}
        <span className="font-mono text-muted-foreground">
          {item.provider} / {item.section}
        </span>
      </div>
      <h2 id="kb-entry" className="text-lg font-semibold break-words">
        {item.title}
      </h2>
      {item.needs_reverify ? (
        <p className="text-sm">
          Code it describes changed{item.reverify_reason ? `: ${item.reverify_reason}` : ""}. Check it against the code, then propose an update or retire it.
        </p>
      ) : null}
      <dl className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[8rem_1fr]">
        <dt className="text-muted-foreground">Path</dt>
        <dd className="font-mono break-all">{item.path}</dd>
        <dt className="text-muted-foreground">Written by</dt>
        <dd className="break-words">
          {p.author_role}
          {p.source ? <span className="font-mono"> ({p.source})</span> : null}
        </dd>
        <dt className="text-muted-foreground">Created</dt>
        <dd>
          <Age iso={p.created} ago />
        </dd>
        <dt className="text-muted-foreground">Last verified</dt>
        <dd>
          <Age iso={p.last_verified} ago />
        </dd>
        {item.scope ? (
          <>
            <dt className="text-muted-foreground">Scope</dt>
            <dd>{item.scope}</dd>
          </>
        ) : null}
      </dl>
      {item.source_url ? <ExternalLink href={item.source_url}>Open the source</ExternalLink> : null}
      {item.body !== undefined ? (
        <Markdown>{item.body}</Markdown>
      ) : (
        <div>
          <p className="text-xs text-muted-foreground">Excerpt</p>
          <p className="text-sm break-words whitespace-pre-wrap">{item.snippet}</p>
        </div>
      )}
    </section>
  )
}

/** The entries as a list and the selected one beside it. */
function Browser({ items, label }: { items: Item[]; label: string }) {
  const [picked, setPicked] = useState("")
  const selected = items.find((i) => i.id === picked) ?? items[0]!
  return (
    <div className="grid grid-cols-1 items-start gap-4 xl:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
      <ul aria-label={label} className="overflow-hidden rounded-xl border bg-card">
        {items.map((i) => (
          <li key={i.id} className="border-b last:border-b-0">
            <button
              type="button"
              aria-current={i.id === selected.id ? "true" : undefined}
              onClick={() => setPicked(i.id)}
              className={cn("block w-full space-y-0.5 border-l-2 border-transparent px-3 py-2 text-left text-sm hover:bg-accent", i.id === selected.id && "border-primary")}
            >
              <span className="flex flex-wrap items-center gap-2">
                <span className="font-medium break-words">{i.title}</span>
                {i.needs_reverify ? <StatusBadge tone="attention">needs re-verify</StatusBadge> : null}
              </span>
              <span className="block font-mono text-xs break-all text-muted-foreground">
                {i.kind} · {i.section} · {i.path}
              </span>
            </button>
          </li>
        ))}
      </ul>
      <EntryView item={selected} />
    </div>
  )
}

function Results({ project, q, section, kind, provider }: { project: string; q: string; section: string; kind: string; provider: string }) {
  const searching = q.trim() !== ""
  const listed = useKnowledge(project, { section, kind: kind as KnowledgeKind | "", provider })
  const found = useKnowledgeSearch(project, q, { section, kind: kind as KnowledgeKind | "", provider })
  return searching ? (
    <Async query={found} isEmpty={(d) => d.length === 0} empty={`Nothing found for “${q}”.`}>
      {(d) => <Browser items={d} label="Search results" />}
    </Async>
  ) : (
    <Async query={listed} isEmpty={(d) => d.entries.length === 0} empty="No entries here yet.">
      {(d) => <Browser items={d.entries} label="Entries" />}
    </Async>
  )
}

function EntriesTab({ project }: { project: string }) {
  const { q = "", section = "", kind = "", provider = "" } = useSearch({ from: "/app/knowledge" })
  const navigate = useNavigate({ from: "/knowledge" })
  const [draft, setDraft] = useState(q)
  const sections = useKnowledge(project, {})
  const providers = [...new Set(sections.data?.sections.map((s) => s.provider).filter(Boolean))] as string[]
  const set = (next: Record<string, string>) =>
    void navigate({ search: (prev) => ({ ...prev, ...Object.fromEntries(Object.entries(next).map(([k, v]) => [k, v || undefined])) }) })
  return (
    <div className="space-y-4">
      <form
        role="search"
        className="flex flex-wrap items-end gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          set({ q: draft.trim() })
        }}
      >
        <div className="min-w-48 flex-1 space-y-1.5">
          <Label htmlFor="kb-q">Search knowledge</Label>
          <Input id="kb-q" value={draft} onChange={(e) => setDraft(e.target.value)} placeholder="words, “phrases”, -exclusions" />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="kb-kind">Kind</Label>
          <NativeSelect id="kb-kind" className="w-full sm:w-40" value={kind} onChange={(e) => set({ kind: e.target.value })}>
            <NativeSelectOption value="">All kinds</NativeSelectOption>
            {KINDS.map((k) => (
              <NativeSelectOption key={k} value={k}>
                {k}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="kb-provider">Provider</Label>
          <NativeSelect id="kb-provider" className="w-full sm:w-40" value={provider} onChange={(e) => set({ provider: e.target.value })}>
            <NativeSelectOption value="">All providers</NativeSelectOption>
            {providers.map((p) => (
              <NativeSelectOption key={p} value={p}>
                {p}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
        <Button type="submit">Search</Button>
        {q ? (
          <Button
            type="button"
            variant="ghost"
            onClick={() => {
              setDraft("")
              set({ q: "" })
            }}
          >
            Clear search
          </Button>
        ) : null}
      </form>
      <div className="grid grid-cols-1 items-start gap-6 lg:grid-cols-[18rem_minmax(0,1fr)]">
        <Async query={sections} isEmpty={() => false}>
          {(d) => <Sections sections={d.sections} section={section} onPick={(name) => set({ section: name })} />}
        </Async>
        <Results project={project} q={q} section={section} kind={kind} provider={provider} />
      </div>
    </div>
  )
}

export function KnowledgePage() {
  const { project: chosen = "", tab = "entries" } = useSearch({ from: "/app/knowledge" })
  const navigate = useNavigate({ from: "/knowledge" })
  const projects = useProjects()
  // The sidebar's project is the filter; with one project there is nothing to choose.
  const project = chosen || (projects.data?.length === 1 ? projects.data[0]!.name : "")
  const active: Tab = TABS.some((t) => t.id === tab) ? (tab as Tab) : "entries"
  return (
    <>
      <PageHeader title="Knowledge" meta={project ? `What agents and people know about ${project}.` : "What agents and people know about a project."} />
      {project === "" ? (
        <p className="rounded-xl border border-dashed bg-card p-6 text-center text-muted-foreground">
          Choose a project in the sidebar to see its knowledge. <Link to="/projects" className="text-primary underline underline-offset-2">Projects</Link>
        </p>
      ) : (
        <div className="space-y-6">
          <div className="flex flex-wrap gap-2" role="group" aria-label="Knowledge views">
            {TABS.map((t) => (
              <Button
                key={t.id}
                size="sm"
                variant={t.id === active ? "default" : "outline"}
                aria-pressed={t.id === active}
                onClick={() => void navigate({ search: (prev) => ({ ...prev, tab: t.id === "entries" ? undefined : t.id }) })}
              >
                {t.label}
              </Button>
            ))}
          </div>
          {active === "entries" ? <EntriesTab project={project} /> : null}
          {active === "proposals" ? <ProposalsTab project={project} /> : null}
          {active === "notes" ? <NotesTab project={project} /> : null}
          {active === "integrations" ? <IntegrationsTab project={project} /> : null}
        </div>
      )}
    </>
  )
}
