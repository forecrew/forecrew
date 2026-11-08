import { Link } from "@tanstack/react-router"
import { useState } from "react"
import { useModelsCatalog, useResources } from "@/api/queries"
import type { ModelAssignmentSpec, Project } from "@/api/types"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { humanName } from "@/lib/format"
import { connectionTitle, connectionsOf, modelOf, organizationAssignment, projectAssignment, providerOf, whereText } from "@/lib/models"
import { PermButton, useCan } from "@/lib/permissions"

/**
 * The project's AI models in Setup: what it inherits from the organization, its own overrides,
 * and which connections may receive its code, with authorize and revoke. Each change is a plan
 * with no pull request: a repo file must not decide cost or where code goes. Sending code to a
 * provider lowers protection, so the plan asks for the second confirmation.
 */
export function ProjectModels({ project }: { project: Project }) {
  const can = useCan()
  // The connections and assignments are the organization's: reading them takes models.read there.
  const read = can("models.read")
  const assign = can("models.assign", project.id)
  const catalog = useModelsCatalog(read)
  const connections = useResources("model_connection", read)
  const assignments = useResources("model_assignment", read)
  const authorizations = useResources("model_authorization", read)
  const draft = useDraft()
  const [role, setRole] = useState("")
  const [connection, setConnection] = useState("")
  if (!read) {
    if (!assign) return null
    return (
      <section aria-labelledby="project-models" className="mt-8 space-y-2">
        <h2 id="project-models" className="text-lg font-semibold">
          Setup · AI models
        </h2>
        <p className="text-sm text-muted-foreground">
          You may choose this project's models, but the AI providers are the organization's and you cannot see them: that needs <code className="font-mono">models.read</code>. Ask an organization admin, or the assistant.
        </p>
      </section>
    )
  }

  const list = connectionsOf(connections.data)
  const org = organizationAssignment(assignments.data)
  const own = projectAssignment(assignments.data, project.name)
  const overridesAllowed = org.spec.project_overrides ?? true
  const effective: ModelAssignmentSpec = { tiers: org.spec.tiers, roles: { ...org.spec.roles, ...own?.roles } }
  const roles = [...new Set([...(catalog.data?.roles ?? []).map((r) => r.name), ...Object.keys(org.spec.roles ?? {})])]
  const names = (authorizations.data ?? []).map((a) => a.name)
  const authorized = (c: string) => names.includes(`${c}@${project.name}`) || names.includes(`${c}@*`)
  const providers = catalog.data?.providers ?? []
  const repo = project.repo

  const override = async (e: React.FormEvent) => {
    e.preventDefault()
    const tier = org.spec.roles?.[role]?.tier ?? catalog.data?.roles.find((r) => r.name === role)?.tier ?? "strong"
    const spec: ModelAssignmentSpec = { roles: { ...own?.roles, [role]: { connection, tier } } }
    await draft.make(`Use ${connection} for ${humanName(role)} in ${project.name}`, [{ kind: "model_assignment", name: `project:${project.name}`, spec }])
  }
  const removeOverride = (r: string) => {
    const rest = Object.fromEntries(Object.entries(own?.roles ?? {}).filter(([k]) => k !== r))
    void draft.make(
      `Put ${humanName(r)} back on the organization's model in ${project.name}`,
      Object.keys(rest).length ? [{ kind: "model_assignment", name: `project:${project.name}`, spec: { roles: rest } }] : [{ kind: "model_assignment", name: `project:${project.name}`, delete: true }],
    )
  }

  return (
    <section aria-labelledby="project-models" className="mt-8 space-y-3">
      <h2 id="project-models" className="text-lg font-semibold">
        Setup · AI models
      </h2>
      <p className="text-sm text-muted-foreground">
        It uses the organization's models (
        <Link to="/settings/models" className="underline underline-offset-2">
          Organization settings, AI models
        </Link>
        ){own?.roles && Object.keys(own.roles).length ? ", with its own choices below" : ""}. Changes here are a plan you confirm, never a pull request.
      </p>
      {draft.plan ? (
        <div className="max-w-2xl">
          <DraftPlan draft={draft} onEdit={() => void draft.discard()} onDone={() => void draft.discard()} />
        </div>
      ) : null}
      <TableWrap label="Models of the project">
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Agent</TableHead>
              <TableHead>Model</TableHead>
              <TableHead>From</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {roles.map((r) => {
              const m = modelOf(effective, r, list)
              const mine = Boolean(own?.roles?.[r])
              return (
                <TableRow key={r}>
                  <TableCell>{humanName(r)}</TableCell>
                  <TableCell>{m ? `${m.model || `no ${m.tier} model`} on ${m.connection}` : "Not set"}</TableCell>
                  <TableCell>
                    {mine ? "This project" : "Organization"}{" "}
                    {mine && assign ? (
                      <Button variant="link" size="sm" className="h-auto p-0" aria-label={`Undo: use the organization's model for ${humanName(r)}`} disabled={draft.busy} onClick={() => removeOverride(r)}>
                        Undo
                      </Button>
                    ) : null}
                  </TableCell>
                </TableRow>
              )
            })}
          </TableBody>
        </Table>
      </TableWrap>
      {overridesAllowed ? (
        assign ? (
          <form onSubmit={(e) => void override(e)} aria-label="Choose a model for this project" className="flex flex-wrap items-end gap-2">
            <Field label="Agent">
              {(p) => (
                <NativeSelect {...p} value={role} onChange={(e) => setRole(e.target.value)}>
                  <NativeSelectOption value="">Choose…</NativeSelectOption>
                  {roles.map((r) => (
                    <NativeSelectOption key={r} value={r}>
                      {humanName(r)}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              )}
            </Field>
            <Field label="Provider">
              {(p) => (
                <NativeSelect {...p} value={connection} onChange={(e) => setConnection(e.target.value)}>
                  <NativeSelectOption value="">Choose…</NativeSelectOption>
                  {list
                    .filter((c) => c.spec.enabled)
                    .map((c) => (
                      <NativeSelectOption key={c.name} value={c.name}>
                        {c.name}
                      </NativeSelectOption>
                    ))}
                </NativeSelect>
              )}
            </Field>
            <Button type="submit" variant="outline" disabled={!role || !connection || draft.busy}>
              Review the change
            </Button>
          </form>
        ) : (
          <PermButton permission="models.assign" project={project.id} variant="outline">
            Choose a model for this project
          </PermButton>
        )
      ) : (
        <p className="text-sm text-muted-foreground">The organization keeps every project on its own models.</p>
      )}
      <h3 className="pt-2 font-medium">Who may receive its code</h3>
      <ul aria-label="Who may receive the code" className="space-y-2 text-sm">
        {list.map((c) => {
          const provider = providerOf(providers, c.spec.vendor)
          const local = c.local
          const title = connectionTitle(c, providers)
          const exempt = local || c.fromFile
          const ok = exempt || authorized(c.name)
          const everyProject = names.includes(`${c.name}@*`)
          return (
            <li key={c.name} className="flex flex-wrap items-center gap-2 rounded-lg border p-2">
              <span className="font-medium">{title}</span>
              <span className="text-muted-foreground">
                {local
                  ? "Local: the code stays on your network"
                  : c.fromFile
                    ? "Server file: allowed"
                    : ok
                      ? `Allowed${everyProject ? " for every project" : ""}: the code of ${repo} goes to ${provider?.title ?? c.name} (${whereText(c, provider)})`
                      : "Not allowed: jobs with private code on it wait"}
              </span>
              {!exempt && assign ? (
                ok ? (
                  everyProject ? null : (
                    <Button
                      variant="outline"
                      size="sm"
                      className="ml-auto"
                      disabled={draft.busy}
                      onClick={() => void draft.make(`Stop sending the code of ${project.name} to ${c.name}`, [{ kind: "model_authorization", name: `${c.name}@${project.name}`, delete: true }])}
                    >
                      Revoke
                    </Button>
                  )
                ) : (
                  <Button
                    variant="outline"
                    size="sm"
                    className="ml-auto"
                    disabled={draft.busy}
                    onClick={() => void draft.make(`Let ${c.name} receive the code of ${project.name}`, [{ kind: "model_authorization", name: `${c.name}@${project.name}`, spec: {} }])}
                  >
                    Authorize
                  </Button>
                )
              ) : null}
            </li>
          )
        })}
      </ul>
      {!assign ? (
        <p className="text-sm text-muted-foreground">
          Authorizing or revoking needs <code className="font-mono">models.assign</code> on this project.
        </p>
      ) : null}
      {draft.error ? (
        <p role="alert" className="text-sm text-destructive">
          {draft.error}
        </p>
      ) : null}
    </section>
  )
}
