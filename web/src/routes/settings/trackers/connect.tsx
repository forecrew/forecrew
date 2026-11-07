import { useState } from "react"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { FORM_KINDS, looksPrivate, type FormKind } from "@/lib/trackers"

type Trigger = "label" | "state" | "assignee"
type JiraAuth = "pat" | "api_token" | "scoped_token"

const NAME = /^[a-z0-9][a-z0-9._-]{0,63}$/
const KEY = /^[A-Z][A-Z0-9_]*$/

/** "ENG, mobile" as ["ENG", "MOBILE"]. */
const keys = (text: string) => text.split(/[\s,]+/).filter(Boolean).map((k) => k.toUpperCase())

/** The words of the one thing a person gives to hand a ticket over. */
const TRIGGERS: Record<Trigger, { label: string; hint: (kind: FormKind) => string; placeholder: string }> = {
  label: { label: "A label is added", hint: () => "A person adds this label to a ticket.", placeholder: "forecrew" },
  state: { label: "It is moved to a status", hint: (k) => (k === "linear" ? "The name of a workflow state, such as Ready for agents." : "The name of a status, such as Ready for agents."), placeholder: "Ready for agents" },
  assignee: { label: "It is assigned to the Forecrew account", hint: (k) => (k === "linear" ? "The user ID of the dedicated Linear user. The connection test prints it." : "The account ID (Cloud) or user key (Data Center) of the dedicated account. The connection test prints it."), placeholder: "" },
}

/**
 * Connects a tracker: the address, where tickets are handed over, and, in the
 * plan this makes, the secure field of the token. Nothing is stored until the plan is confirmed.
 */
export function ConnectTracker({
  open, onOpenChange, taken, initialKind, initialHost, onConnected,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  /** Names of the connections that exist. */
  taken: string[]
  initialKind?: FormKind
  initialHost?: string
  /** Called with the name once the plan is applied and the dialog is done. */
  onConnected: (name: string) => void
}) {
  const [kind, setKind] = useState<FormKind>(initialKind ?? "jira")
  const [name, setName] = useState<string>()
  const [url, setUrl] = useState(initialHost ? `https://${initialHost}` : "")
  const [projects, setProjects] = useState("")
  const [auth, setAuth] = useState<JiraAuth>("pat")
  const [email, setEmail] = useState("")
  const [teams, setTeams] = useState("")
  const [trigger, setTrigger] = useState<Trigger>("label")
  const [value, setValue] = useState("forecrew")
  const [webhook, setWebhook] = useState(false)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const draft = useDraft()
  // A second Jira is jira-2: the name a person has not typed is one that is free.
  const free = [kind, ...Array.from({ length: 20 }, (_, i) => `${kind}-${i + 2}`)].find((n) => !taken.includes(n)) ?? kind
  const chosen = name ?? free

  const reset = () => {
    setName(undefined)
    setUrl("")
    setProjects("")
    setEmail("")
    setTeams("")
    setValue("forecrew")
    setTrigger("label")
    setWebhook(false)
    setErrors({})
    void draft.discard()
  }
  const close = (next: boolean) => {
    onOpenChange(next)
    if (!next) reset()
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const next: Record<string, string> = {}
    if (!NAME.test(chosen)) next.name = "Use lowercase letters, digits, dots, dashes and underscores."
    else if (taken.includes(chosen)) next.name = "A connection has this name already."
    if (kind === "jira") {
      if (!/^https:\/\/[^\s/]+(\/\S*)?$/.test(url.trim())) next.url = "Enter the https address of Jira, with its context path if it has one."
      const list = keys(projects)
      if (list.length === 0 || list.some((k) => !KEY.test(k))) next.projects = "Enter the project keys, such as ENG, MOBILE."
      if (auth !== "pat" && !/^[^@\s]+@[^@\s]+$/.test(email.trim())) next.email = "Enter the email of the account the token belongs to."
    } else {
      const list = keys(teams)
      if (list.length === 0 || list.some((k) => !/^[A-Z0-9]+$/.test(k))) next.teams = "Enter the team keys, such as ENG."
    }
    if (!value.trim()) next.value = "Say what hands a ticket over."
    setErrors(next)
    if (Object.keys(next).length) return
    const token = `store:${chosen}-token`
    const hook = webhook ? { webhook_secret_ref: `store:${chosen}-webhook` } : {}
    const config =
      kind === "jira"
        ? { base_url: url.trim().replace(/\/+$/, ""), projects: keys(projects), auth, ...(auth !== "pat" ? { email: email.trim() } : {}), token_ref: token, ...hook }
        : { teams: keys(teams), token_ref: token, ...hook }
    await draft.make(`Connect ${kind === "jira" ? "Jira" : "Linear"} as ${chosen}`, [
      { kind: "tracker_connection", name: chosen, spec: { kind, config, triggers: { [trigger]: value.trim() } } },
    ])
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Connect a tracker</DialogTitle>
          <DialogDescription>Tickets handed to Forecrew become goals, and their status goes back to the ticket. You confirm the change before anything is saved.</DialogDescription>
        </DialogHeader>
        {draft.plan ? (
          <DraftPlan
            draft={draft}
            onEdit={() => void draft.discard()}
            onDone={() => {
              onConnected(chosen)
              close(false)
            }}
          />
        ) : (
          <form onSubmit={(e) => void submit(e)} noValidate aria-label="Connect a tracker" className="space-y-3">
            <Field label="Tracker">
              {(p) => (
                <NativeSelect {...p} value={kind} onChange={(e) => setKind(e.target.value as FormKind)}>
                  {FORM_KINDS.map((k) => (
                    <NativeSelectOption key={k.value} value={k.value}>
                      {k.label}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              )}
            </Field>
            {kind === "jira" ? (
              <>
                <Field label="Address" error={errors.url} hint="For example https://jira.acme.local/jira or https://acme.atlassian.net">
                  {(p) => <Input {...p} inputMode="url" value={url} onChange={(e) => setUrl(e.target.value)} />}
                </Field>
                {looksPrivate(url) ? (
                  <p role="note" className="rounded-lg border border-warning/50 p-3 text-sm">
                    This looks like a private address. A hosted Forecrew cannot reach it: Jira must be reachable from outside. A self-hosted Forecrew in the same network can, once its admin turns on <code className="font-mono">trackers.private_addresses</code>.
                  </p>
                ) : null}
                <Field label="Projects" error={errors.projects} hint="The keys of the Jira projects to read, separated by commas.">
                  {(p) => <Input {...p} className="font-mono" value={projects} onChange={(e) => setProjects(e.target.value)} />}
                </Field>
                <Field label="Token">
                  {(p) => (
                    <NativeSelect {...p} value={auth} onChange={(e) => setAuth(e.target.value as JiraAuth)}>
                      <NativeSelectOption value="pat">Jira Data Center: personal access token</NativeSelectOption>
                      <NativeSelectOption value="api_token">Jira Cloud: API token</NativeSelectOption>
                      <NativeSelectOption value="scoped_token">Jira Cloud: API token with scopes</NativeSelectOption>
                    </NativeSelect>
                  )}
                </Field>
                {auth !== "pat" ? (
                  <Field label="Account email" error={errors.email} hint="The Atlassian account the token belongs to.">
                    {(p) => <Input {...p} type="email" value={email} onChange={(e) => setEmail(e.target.value)} />}
                  </Field>
                ) : null}
              </>
            ) : (
              <Field label="Teams" error={errors.teams} hint="The keys of the Linear teams to read, separated by commas.">
                {(p) => <Input {...p} className="font-mono" value={teams} onChange={(e) => setTeams(e.target.value)} />}
              </Field>
            )}
            <p className="text-sm text-muted-foreground">
              Make a dedicated account for Forecrew in {kind === "jira" ? "Jira" : "Linear"} and a token for it. You type the token in a secure field on the next step: it goes straight to secure storage.
            </p>
            <Field label="A person hands a ticket over when">
              {(p) => (
                <NativeSelect
                  {...p}
                  value={trigger}
                  onChange={(e) => {
                    setTrigger(e.target.value as Trigger)
                    setValue(e.target.value === "label" ? "forecrew" : "")
                  }}
                >
                  {(Object.keys(TRIGGERS) as Trigger[]).map((t) => (
                    <NativeSelectOption key={t} value={t}>
                      {TRIGGERS[t].label}
                    </NativeSelectOption>
                  ))}
                </NativeSelect>
              )}
            </Field>
            <Field label={trigger === "label" ? "Label" : trigger === "state" ? "Status" : "Account ID"} error={errors.value} hint={TRIGGERS[trigger].hint(kind)}>
              {(p) => <Input {...p} placeholder={TRIGGERS[trigger].placeholder} value={value} onChange={(e) => setValue(e.target.value)} />}
            </Field>
            <details className="space-y-3 text-sm">
              <summary className="cursor-pointer text-muted-foreground">More options</summary>
              <div className="space-y-3 pt-3">
                <Field label="Name" error={errors.name} hint="How the connection is called here and in commands.">
                  {(p) => <Input {...p} className="font-mono" value={chosen} onChange={(e) => setName(e.target.value)} />}
                </Field>
                <label className="flex items-start gap-2">
                  <input type="checkbox" className="mt-1" checked={webhook} onChange={(e) => setWebhook(e.target.checked)} />
                  <span>
                    Use webhooks, which are faster. You give the signing secret in a second secure field, then register Forecrew's address in {kind === "jira" ? "Jira" : "Linear"}. Without them Forecrew looks every two minutes.
                  </span>
                </label>
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
