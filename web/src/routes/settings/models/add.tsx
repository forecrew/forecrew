import { useState } from "react"
import type { ModelConnectionSpec, ModelProvider } from "@/api/types"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { CONNECTION_NAME, KEY_ENV, countryName, localAddress } from "@/lib/models"

const OTHER = "other"

/** Where a catalog provider runs, in a few words. */
const runsIn = (p: ModelProvider) => (p.country === "local" ? "on your own machine" : p.country === "region" ? "in the region you pick" : `in ${countryName(p.country)}`)

/**
 * Adds a provider: pick it from the catalog, or another endpoint by its address; the plan then
 * asks for the key in its secure field, and a local endpoint needs none. Nothing is stored until
 * the plan is confirmed; saving it checks the key.
 */
export function AddProvider({ open, onOpenChange, providers, taken }: { open: boolean; onOpenChange: (open: boolean) => void; providers: ModelProvider[]; taken: string[] }) {
  const [choice, setChoice] = useState("")
  const [name, setName] = useState<string>()
  const [address, setAddress] = useState<string>()
  const [keyEnv, setKeyEnv] = useState<string>()
  const [errors, setErrors] = useState<Record<string, string>>({})
  const draft = useDraft()
  const provider = providers.find((p) => p.name === choice)
  const other = choice === OTHER
  const base = provider?.name ?? "endpoint"
  const free = [base, ...Array.from({ length: 20 }, (_, i) => `${base}-${i + 2}`)].find((n) => !taken.includes(n)) ?? base
  const chosenName = name ?? free
  const url = address ?? provider?.base_url ?? ""
  const local = localAddress(url) || provider?.country === "local"
  const variable = keyEnv ?? provider?.key_env ?? ""

  const close = (next: boolean) => {
    onOpenChange(next)
    if (next) return
    setChoice("")
    setName(undefined)
    setAddress(undefined)
    setKeyEnv(undefined)
    setErrors({})
    void draft.discard()
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const next: Record<string, string> = {}
    if (!choice) next.choice = "Pick a provider, or another endpoint."
    if (!CONNECTION_NAME.test(chosenName)) next.name = "Use lowercase letters, digits and -."
    else if (taken.includes(chosenName)) next.name = "A connection has this name already."
    if ((other || address !== undefined) && !/^https?:\/\/[^\s/]+(\/\S*)?$/.test(url.trim())) next.address = "Enter the address of the API, such as https://models.acme.com/v1."
    else if (url.startsWith("http://") && !localAddress(url)) next.address = "Plain http only reaches your own network: use https."
    const adapter = provider?.adapter ?? "opencode"
    if (adapter === "opencode" && !KEY_ENV.test(variable)) next.keyEnv = "The variable the agent reads the key from: uppercase, ending in _KEY."
    setErrors(next)
    if (Object.keys(next).length) return
    const spec: ModelConnectionSpec = { vendor: provider?.name ?? chosenName, adapter, enabled: true }
    if (url) spec.base_url = url.trim().replace(/\/+$/, "")
    if (adapter === "opencode") spec.key_env = variable
    if (!local) spec.secret_ref = `store:models/${chosenName}`
    await draft.make(`Add AI provider ${provider?.title ?? chosenName}`, [{ kind: "model_connection", name: chosenName, spec }])
  }

  return (
    <Dialog open={open} onOpenChange={close}>
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Add an AI provider</DialogTitle>
          <DialogDescription>Pick where your models come from. The key goes in a secure field on the next step; you confirm before anything is saved.</DialogDescription>
        </DialogHeader>
        {draft.plan ? (
          <DraftPlan draft={draft} onEdit={() => void draft.discard()} onDone={() => close(false)} />
        ) : (
          <form onSubmit={(e) => void submit(e)} noValidate aria-label="Add an AI provider" className="space-y-4">
            <fieldset className="space-y-2" aria-describedby={errors.choice ? "provider-error" : undefined}>
              <legend className="mb-2 text-sm font-medium">Provider</legend>
              <div className="grid gap-2 sm:grid-cols-2">
                {[...providers, undefined].map((p) => {
                  const value = p?.name ?? OTHER
                  return (
                    <label key={value} className="flex cursor-pointer items-start gap-2 rounded-lg border p-3 text-sm has-[:checked]:border-primary has-[:checked]:bg-accent/50">
                      <input
                        type="radio"
                        name="provider"
                        className="mt-1"
                        value={value}
                        checked={choice === value}
                        onChange={() => {
                          setChoice(value)
                          setName(undefined)
                          setAddress(undefined)
                          setKeyEnv(undefined)
                        }}
                      />
                      <span>
                        <span className="block font-medium">{p?.title ?? "Another endpoint"}</span>
                        <span className="text-muted-foreground">{p ? `Runs ${runsIn(p)}` : "Any API compatible with OpenAI, by its address"}</span>
                      </span>
                    </label>
                  )
                })}
              </div>
              {errors.choice ? (
                <p id="provider-error" className="text-sm text-destructive">
                  {errors.choice}
                </p>
              ) : null}
            </fieldset>
            {other ? (
              <Field label="Address" error={errors.address} hint="The API's base address, for example https://models.acme.com/v1 or http://localhost:11434/v1.">
                {(p) => <Input {...p} inputMode="url" value={url} onChange={(e) => setAddress(e.target.value)} />}
              </Field>
            ) : null}
            {choice ? (
              <p className="text-sm text-muted-foreground">
                {local ? "No key needed: the model runs on your own network, and the code stays there." : "Next you type the key in a secure field: it goes straight to secure storage and is never shown again."}
              </p>
            ) : null}
            {choice ? (
              <details className="space-y-3 text-sm" open={Boolean(errors.name || errors.keyEnv || (errors.address && !other))}>
                <summary className="cursor-pointer text-muted-foreground">More options</summary>
                <div className="space-y-3 pt-3">
                  <Field label="Name" error={errors.name} hint="How the connection is called here and in commands.">
                    {(p) => <Input {...p} className="font-mono" value={chosenName} onChange={(e) => setName(e.target.value)} />}
                  </Field>
                  {!other ? (
                    <Field label="Address" error={errors.address} hint="Change it for a proxy or a regional endpoint.">
                      {(p) => <Input {...p} inputMode="url" className="font-mono" value={url} onChange={(e) => setAddress(e.target.value)} />}
                    </Field>
                  ) : null}
                  {(provider?.adapter ?? "opencode") === "opencode" ? (
                    <Field label="Key variable" error={errors.keyEnv} hint="The variable the agent reads the key from.">
                      {(p) => <Input {...p} className="font-mono" value={variable} onChange={(e) => setKeyEnv(e.target.value)} />}
                    </Field>
                  ) : null}
                  <p className="text-muted-foreground">Agent: {provider?.adapter ?? "opencode"}. Forecrew picks it for the provider.</p>
                </div>
              </details>
            ) : null}
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
