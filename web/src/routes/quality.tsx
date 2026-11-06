import { useSearch } from "@tanstack/react-router"
import { useProjects, useQuality } from "@/api/queries"
import type { QualityCell, VerificationInfo } from "@/api/types"
import { Async } from "@/components/Async"
import { CellGrid, FailureList, totals } from "@/components/CellGrid"
import { ExternalLink } from "@/components/ExternalLink"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { VerificationPanel } from "@/components/Verification"
import { count } from "@/lib/format"

/** The flaky tests of a report per environment name. */
function flakyByEnv(v?: VerificationInfo): Record<string, number> {
  const out: Record<string, number> = {}
  for (const f of v?.report.flaky ?? []) for (const e of f.environments) out[e] = (out[e] ?? 0) + 1
  return out
}

function Subject({ id, title, cells, verification, children }: { id: string; title: React.ReactNode; cells: QualityCell[]; verification?: VerificationInfo; children?: React.ReactNode }) {
  const t = totals(cells)
  // Flakiness is known only from a report; a report of an earlier commit says nothing about these cells.
  const flaky = verification?.report.flaky.length
  const missing = verification?.current ? verification.report.results.filter((x) => x.status === "missing") : []
  return (
    <article aria-labelledby={id} className="space-y-3 rounded-xl border bg-card p-4">
      <div className="flex flex-wrap items-center gap-2">
        <h3 id={id} className="font-medium">
          {title}
        </h3>
        {children}
      </div>
      {cells.length === 0 && missing.length === 0 ? (
        <p className="text-sm text-muted-foreground">No test results yet.</p>
      ) : (
        <>
          <p className="text-sm">
            {count(t.passed)} passed, {count(t.failed)} failed, {count(t.skipped)} skipped{flaky === undefined ? "" : `, ${flaky} flaky`}
          </p>
          <CellGrid cells={cells} label={`${id} results`} flaky={flakyByEnv(verification)} missing={missing} />
          <FailureList cells={cells} label={`${id} failing tests`} />
        </>
      )}
      {verification ? <VerificationPanel info={verification} title={`${id} verification`} /> : null}
    </article>
  )
}

function ProjectQuality({ name }: { name: string }) {
  const q = useQuality(name)
  return (
    <section aria-labelledby={`project-${name}`} className="mb-8 space-y-3">
      <h2 id={`project-${name}`} className="text-lg font-semibold">
        {name}
      </h2>
      <Async query={q}>
        {(d) => (
          <div className="space-y-4">
            {d.baseline ? (
              <Subject id={`${name}-baseline`} title={<>Latest run outside a pull request</>} cells={d.baseline.cells}>
                <span className="font-mono text-xs text-muted-foreground">{d.baseline.sha.slice(0, 7)}</span>
              </Subject>
            ) : (
              <p className="rounded-xl border border-dashed bg-card p-4 text-sm text-muted-foreground">
                No baseline yet: no run outside a pull request.
              </p>
            )}
            {d.changes.length === 0 ? <p className="text-sm text-muted-foreground">No open pull requests.</p> : null}
            {d.changes.map((c) => (
              <Subject
                key={c.number}
                id={`${name}-pr-${c.number}`}
                title={
                  <>
                    Pull request #{c.number}
                    {c.task ? <span className="ml-2 font-mono text-sm font-normal text-muted-foreground">{c.task}</span> : null}
                  </>
                }
                cells={c.cells}
                verification={c.verification}
              >
                <span className="font-mono text-xs text-muted-foreground">{c.sha.slice(0, 7)}</span>
                {c.verification ? null : <StatusBadge>no verification report</StatusBadge>}
                <ExternalLink href={c.url} className="ml-auto text-sm">
                  Open on GitHub
                </ExternalLink>
              </Subject>
            ))}
          </div>
        )}
      </Async>
    </section>
  )
}

/** Test health per project: the latest run outside a pull request (the baseline) and every open pull request. The project comes from the sidebar switcher. */
export function QualityPage() {
  const { project = "" } = useSearch({ from: "/app/quality" })
  const projects = useProjects()
  return (
    <>
      <PageHeader title="Quality" />
      <Async query={projects} isEmpty={(d) => d.length === 0} empty="No projects yet. Onboard a repository to see its tests.">
        {(all) => {
          const shown = project ? all.filter((p) => p.name === project) : all
          return shown.length === 0 ? (
            <p className="text-muted-foreground">Unknown project.</p>
          ) : (
            shown.map((p) => <ProjectQuality key={p.id} name={p.name} />)
          )
        }}
      </Async>
    </>
  )
}
