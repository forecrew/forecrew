import { useState } from "react"
import { useModelChecks, useModelsCatalog, useResources } from "@/api/queries"
import { Async } from "@/components/Async"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { YamlDialog } from "@/components/YamlDialog"
import { connectionsOf, organizationAssignment } from "@/lib/models"
import { PermButton, useCan } from "@/lib/permissions"
import { toYaml } from "@/lib/yaml"
import { AddProvider } from "./models/add"
import { ConnectionRow } from "./models/connection"
import { WhoUsesWhat } from "./models/roles"
import { Section } from "./layout"

/**
 * AI models: the one place for keys. Enter a key and the rest follows: the key is checked, its
 * models are listed and tested, a preset fills the roles. Every change is a plan; keys only go in
 * the plan's secure field.
 */
export function ModelsPage() {
  const can = useCan()
  const read = can("models.read")
  const catalog = useModelsCatalog(read)
  const connections = useResources("model_connection", read)
  const assignments = useResources("model_assignment", read)
  const authorizations = useResources("model_authorization", read)
  const checks = useModelChecks(read)
  const draft = useDraft()
  const [adding, setAdding] = useState(false)
  const list = connectionsOf(connections.data)
  const org = organizationAssignment(assignments.data)
  const yaml = toYaml([...(connections.data ?? []), ...(assignments.data ?? [])].map((r) => ({ kind: r.kind, name: r.name, spec: r.spec })))

  return (
    <Section
      permission="models.read"
      title="AI models"
      meta="Enter a key, and the rest follows: the models are found, tested and given to the roles."
      actions={<YamlDialog title="AI models as YAML" yaml={yaml} label="⋯ YAML" />}
    >
      {draft.plan ? (
        <div className="mb-6 max-w-2xl">
          <DraftPlan draft={draft} onEdit={() => void draft.discard()} onDone={() => void draft.discard()} />
        </div>
      ) : null}
      <section aria-labelledby="connected" className="mb-8 space-y-3">
        <div className="flex flex-wrap items-center gap-2">
          <h2 id="connected" className="text-lg font-semibold">
            Connected
          </h2>
          <PermButton permission="models.manage" className="ml-auto" disabled={!catalog.data} onClick={() => setAdding(true)}>
            Add provider
          </PermButton>
        </div>
        <Async query={connections} isEmpty={(d) => d.length === 0} empty="Add an AI provider key so agents can work. One is enough.">
          {() => (
            <ul className="space-y-2">
              {list.map((c) => (
                <ConnectionRow
                  key={c.name}
                  connection={c}
                  providers={catalog.data?.providers ?? []}
                  checks={checks.data}
                  authorizations={(authorizations.data ?? []).map((a) => a.name)}
                  draft={draft}
                />
              ))}
            </ul>
          )}
        </Async>
        {draft.error ? (
          <p role="alert" className="text-sm text-destructive">
            {draft.error}
          </p>
        ) : null}
      </section>
      {list.length > 0 && catalog.data ? (
        <WhoUsesWhat roles={catalog.data.roles} assignment={org.spec} fromFile={org.fromFile} connections={list} checks={checks.data} draft={draft} />
      ) : null}
      {catalog.data ? (
        <AddProvider open={adding} onOpenChange={setAdding} providers={catalog.data.providers} taken={list.map((c) => c.name)} />
      ) : null}
    </Section>
  )
}
