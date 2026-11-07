import { useQueries } from "@tanstack/react-query"
import { useNavigate, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { api } from "@/api/client"
import { keys, useProjects, useTrackerConnections, useTrackerRoutes, useUnmappedPeople, useWaitingTickets } from "@/api/queries"
import type { TrackerConnection, TrackerTest } from "@/api/types"
import { Async } from "@/components/Async"
import { StatusBadge } from "@/components/StatusBadge"
import { PermButton, useCan } from "@/lib/permissions"
import { FORM_KINDS, kindLabel, type FormKind } from "@/lib/trackers"
import { Section } from "./layout"
import { TrackerCard } from "./trackers/card"
import { ConnectTracker } from "./trackers/connect"

/** A tool the repos mention that is a tracker, not connected: Jira keys in PRs, a Linear link in the README. */
function useDiscovered(connections: TrackerConnection[] | undefined) {
  const projects = useProjects()
  const details = useQueries({
    queries: (projects.data ?? []).slice(0, 12).map((p) => ({ queryKey: keys.project(p.name), queryFn: () => api.getProject(p.name) })),
  })
  const found = new Map<string, { kind: FormKind; instance: string; project: string; confidence: string }>()
  for (const d of details) {
    for (const i of d.data?.profile?.integrations ?? []) {
      const kind = FORM_KINDS.find((k) => k.value === i.tool.toLowerCase())?.value
      if (!kind || i.status === "connected" || connections === undefined || connections.some((c) => c.kind === kind)) continue
      found.set(`${kind}/${i.instance ?? ""}`, { kind, instance: i.instance ?? "", project: d.data?.name ?? "", confidence: i.confidence })
    }
  }
  return [...found.values()]
}

/** Integrations: the trackers Forecrew reads tickets from, and where their tickets go. */
export function IntegrationsPage() {
  const can = useCan()
  const search = useSearch({ strict: false }) as { connect?: string; instance?: string }
  const navigate = useNavigate()
  const connections = useTrackerConnections()
  const manage = can("trackers.manage")
  const routes = useTrackerRoutes()
  const people = useUnmappedPeople(manage)
  const tickets = useWaitingTickets()
  const discovered = useDiscovered(connections.data)
  const initialKind = FORM_KINDS.find((k) => k.value === search.connect)?.value
  const [connecting, setConnecting] = useState<{ kind?: FormKind; host?: string } | undefined>(initialKind ? { kind: initialKind, host: search.instance } : undefined)
  const [tests, setTests] = useState<Record<string, { result: TrackerTest; at: string } | undefined>>({})
  const [testing, setTesting] = useState<string>()
  const [testError, setTestError] = useState<Record<string, string>>({})

  const test = async (name: string) => {
    setTesting(name)
    setTestError((e) => ({ ...e, [name]: "" }))
    try {
      const result = await api.testTrackerConnection(name)
      setTests((t) => ({ ...t, [name]: { result, at: new Date().toISOString() } }))
    } catch (err) {
      setTestError((e) => ({ ...e, [name]: err instanceof Error ? err.message : "The test could not run" }))
    } finally {
      setTesting(undefined)
    }
  }
  const closeConnect = (open: boolean) => {
    if (open) return
    setConnecting(undefined)
    if (search.connect) void navigate({ to: "/settings/integrations", search: {}, replace: true })
  }

  return (
    <Section
      permission="trackers.read"
      title="Integrations"
      meta="Trackers hand tickets to agents from where your team already works."
      actions={
        <PermButton permission="trackers.manage" onClick={() => setConnecting({})}>
          Connect a tracker
        </PermButton>
      }
    >
      {discovered.length > 0 ? (
        <section aria-labelledby="found" className="mb-6 space-y-2">
          <h2 id="found" className="text-lg font-semibold">
            Found in your repos, not connected
          </h2>
          <ul className="space-y-2">
            {discovered.map((d) => (
              <li key={`${d.kind}/${d.instance}`} className="flex flex-wrap items-center justify-between gap-2 rounded-xl border bg-card p-3 text-sm">
                <span>
                  <span className="font-medium">{kindLabel(d.kind)}</span> {d.instance ? <span className="font-mono text-muted-foreground">{d.instance}</span> : null}{" "}
                  <span className="text-muted-foreground">
                    seen in {d.project} · {d.confidence} confidence · Issues
                  </span>
                </span>
                <PermButton permission="trackers.manage" variant="outline" aria-label={`Connect ${kindLabel(d.kind)}`} onClick={() => setConnecting({ kind: d.kind, host: d.instance })}>
                  Connect
                </PermButton>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
      <section aria-labelledby="trackers" className="space-y-3">
        <h2 id="trackers" className="text-lg font-semibold">
          Trackers
        </h2>
        <Async query={connections} isEmpty={(d) => d.length === 0} empty="Connect a tracker and hand tickets to agents from where you already work.">
          {(items) => (
            <div className="space-y-4">
              {items.map((c) => (
                <TrackerCard
                  key={c.id}
                  connection={c}
                  routes={routes.data ?? []}
                  people={(people.data?.people ?? []).filter((p) => p.connection === c.name)}
                  tickets={(tickets.data ?? []).filter((t) => t.connection === c.name)}
                  test={tests[c.name]}
                  testing={testing === c.name}
                  testError={testError[c.name]}
                  onTest={() => void test(c.name)}
                />
              ))}
            </div>
          )}
        </Async>
        {people.data?.problems?.length ? (
          <p role="status" className="text-sm text-muted-foreground">
            <StatusBadge tone="attention">Could not read some tickets</StatusBadge> {people.data.problems.join(" ")}
          </p>
        ) : null}
        {!manage ? <p className="text-sm text-muted-foreground">You can read this page. Connecting a tracker, adding rules and matching people need <code className="font-mono">trackers.manage</code>.</p> : null}
      </section>
      <ConnectTracker
        key={`${connecting?.kind ?? ""}${connecting?.host ?? ""}`}
        open={connecting !== undefined}
        onOpenChange={closeConnect}
        taken={(connections.data ?? []).map((c) => c.name)}
        initialKind={connecting?.kind}
        initialHost={connecting?.host}
        onConnected={(name) => void test(name)}
      />
    </Section>
  )
}
