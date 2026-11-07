import { useQuery } from "@tanstack/react-query"
import { useId, useState } from "react"
import { api } from "@/api/client"
import { useProjects } from "@/api/queries"
import type { TicketState, TrackerConnection, TrackerRoute, TrackerState } from "@/api/types"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { MAPPED_STATES, scopesOf, stateWords, suggestStates, triggerWords } from "@/lib/trackers"

const list = (text: string) => text.split(",").map((s) => s.trim()).filter(Boolean)

/**
 * Adds a rule: which tickets of the tracker go to which project. The trigger is the
 * connection's own; the status mapping is suggested from the tracker's statuses and sits under More options.
 */
export function AddRule({
  open, onOpenChange, connection, routes, states: tested,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  connection: TrackerConnection
  routes: TrackerRoute[]
  /** The tracker's statuses, when a test read them. */
  states: TrackerState[] | undefined
}) {
  const projects = useProjects()
  const scopes = scopesOf(connection)
  const [scope, setScope] = useState(scopes[0] ?? "")
  const [project, setProject] = useState("")
  const [types, setTypes] = useState("")
  const [labels, setLabels] = useState("")
  const [mapping, setMapping] = useState<Partial<Record<TicketState, string>>>()
  const [error, setError] = useState("")
  const draft = useDraft()
  const listId = useId()
  const position = Math.max(0, ...routes.filter((r) => r.connection === connection.name).map((r) => r.position)) + 1

  // The tracker's statuses come from a test of the connection: read once, when the dialog opens, unless a test gave them.
  const read = useQuery({
    queryKey: ["tracker-states", connection.name],
    queryFn: () => api.testTrackerConnection(connection.name),
    enabled: open && !tested,
    retry: false,
    staleTime: Infinity,
  })
  const states = tested ?? read.data?.states
  const reading = read.isFetching
  const mapped = mapping ?? suggestStates(states ?? [])

  const close = (next: boolean) => {
    onOpenChange(next)
    if (!next) {
      setProject("")
      setTypes("")
      setLabels("")
      setMapping(undefined)
      setError("")
      void draft.discard()
    }
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!project) {
      setError("Choose the Forecrew project the tickets go to.")
      return
    }
    setError("")
    const match = { ...(scope ? { project: scope } : {}), ...(list(types).length ? { types: list(types) } : {}), ...(list(labels).length ? { labels: list(labels) } : {}) }
    const states = Object.fromEntries(Object.entries(mapped).filter(([, v]) => v))
    await draft.make(`Add a rule to ${connection.name}`, [
      { kind: "tracker_route", name: `${connection.name}/${position}`, spec: { project, match, ...(Object.keys(states).length ? { states } : {}) } },
    ])
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Add a rule</DialogTitle>
          <DialogDescription>Tickets {triggerWords(connection.triggers)} on {connection.name} that match go to the project you choose.</DialogDescription>
        </DialogHeader>
        {draft.plan ? (
          <DraftPlan draft={draft} onEdit={() => void draft.discard()} onDone={() => close(false)} />
        ) : (
          <form onSubmit={(e) => void submit(e)} noValidate aria-label="Add a rule" className="space-y-3">
            {scopes.length > 0 ? (
              <Field label={connection.kind === "linear" ? "Linear team" : connection.kind === "github" ? "Repository" : "Jira project"}>
                {(p) => (
                  <NativeSelect {...p} value={scope} onChange={(e) => setScope(e.target.value)}>
                    {scopes.map((s) => (
                      <NativeSelectOption key={s} value={s}>
                        {s}
                      </NativeSelectOption>
                    ))}
                  </NativeSelect>
                )}
              </Field>
            ) : null}
            <Field label="Goes to project" error={error}>
              {(p) => (
                <NativeSelect {...p} value={project} onChange={(e) => setProject(e.target.value)}>
                  <NativeSelectOption value="">Choose a project</NativeSelectOption>
                  {projects.data?.filter((x) => x.status === "onboarded").map((x) => (
                    <NativeSelectOption key={x.id} value={x.name}>
                      {x.name}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              )}
            </Field>
            <details className="space-y-3 text-sm">
              <summary className="cursor-pointer text-muted-foreground">More options</summary>
              <div className="space-y-3 pt-3">
                <Field label="Only these types" hint="Separated by commas, such as Bug. Empty takes every type. A bug runs the bug fix flow.">
                  {(p) => <Input {...p} value={types} onChange={(e) => setTypes(e.target.value)} />}
                </Field>
                <Field label="Only these labels" hint="Separated by commas. A ticket needs one of them.">
                  {(p) => <Input {...p} value={labels} onChange={(e) => setLabels(e.target.value)} />}
                </Field>
                <fieldset className="space-y-2">
                  <legend className="text-sm font-medium">Status on the ticket</legend>
                  <p className="text-xs text-muted-foreground">
                    {reading ? "Reading the statuses of the tracker…" : states ? "Suggested from the statuses the tracker has. Leave one empty to leave the ticket's status alone." : "The statuses could not be read: type them as the tracker names them."}
                  </p>
                  <datalist id={listId}>
                    {(states ?? []).map((s) => (
                      <option key={s.name} value={s.name} />
                    ))}
                  </datalist>
                  {MAPPED_STATES.map((s) => (
                    <Field key={s} label={`When the goal is ${stateWords[s]}`} hint={s === "done" || s === "canceled" ? "A finished status." : undefined}>
                      {(p) => <Input {...p} list={listId} value={mapped[s] ?? ""} onChange={(e) => setMapping({ ...mapped, [s]: e.target.value })} />}
                    </Field>
                  ))}
                </fieldset>
              </div>
            </details>
            {draft.error ? (
              <p role="alert" className="text-sm text-destructive">
                {draft.error}
              </p>
            ) : null}
            <Button type="submit" disabled={draft.busy}>
              Review the change
            </Button>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}
