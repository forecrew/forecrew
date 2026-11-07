import { useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { api } from "@/api/client"
import { keys } from "@/api/queries"
import type { ModelCheck, ModelProvider } from "@/api/types"
import type { useDraft } from "@/components/config/draft"
import { ManagedBy } from "@/components/ManagedBy"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { PermButton, useCan } from "@/lib/permissions"
import { connectionTitle, keyAdvice, keyCheckOf, keyStatus, levelText, localAddress, providerOf, qualifiedModel, testsOf, whereText, type Connection } from "@/lib/models"
import { TestModel } from "./test"

/** Which projects may send their private code through a connection, in words. */
function allowedText(c: Connection, authorizations: string[], local: boolean): string {
  if (c.fromFile) return "Server file"
  if (local) return "Local, code stays here"
  const mine = authorizations.filter((a) => a.startsWith(`${c.name}@`))
  if (mine.includes(`${c.name}@*`)) return "Allowed on every project"
  if (mine.length === 0) return "No private code yet"
  return `Allowed on ${mine.length} ${mine.length === 1 ? "project" : "projects"}`
}

/**
 * One connection: what it is, whether its key works, where its models run and who may send it
 * code, with its models and what each may do. Names a provider listed are its text, shown as text.
 */
export function ConnectionRow({
  connection: c, providers, checks, authorizations, draft,
}: {
  connection: Connection
  providers: ModelProvider[]
  checks: ModelCheck[] | undefined
  authorizations: string[]
  draft: ReturnType<typeof useDraft>
}) {
  const qc = useQueryClient()
  const can = useCan()
  const [checking, setChecking] = useState(false)
  const [error, setError] = useState("")
  const provider = providerOf(providers, c.spec.vendor)
  const check = keyCheckOf(checks, c.name)
  const tests = testsOf(checks, c.name)
  const status = keyStatus(check)
  const local = localAddress(c.spec.base_url) || provider?.country === "local"
  const listed = check?.key?.models ?? []
  const models = [...new Set([...tests.map((t) => t.model ?? ""), ...listed.map((m) => qualifiedModel(c, m))])].filter(Boolean)
  const title = connectionTitle(c, providers)
  const where = whereText(c, provider)
  const manage = can("models.manage") && !c.fromFile

  const recheck = async () => {
    setChecking(true)
    setError("")
    try {
      await api.checkModelConnection(c.name)
      await qc.invalidateQueries({ queryKey: keys.modelChecks })
    } catch (err) {
      setError(err instanceof Error ? err.message : "The check could not start")
    } finally {
      setChecking(false)
    }
  }
  const change = (enabled: boolean) =>
    draft.make(`${enabled ? "Turn on" : "Turn off"} AI provider ${title}`, [{ kind: "model_connection", name: c.name, spec: { ...c.spec, enabled } }])

  return (
    <li aria-label={`${title}`} className="rounded-xl border bg-card p-3">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
        <span className="min-w-36 font-medium">{title}</span>
        <span className="text-muted-foreground">
          {models.length} {models.length === 1 ? "model" : "models"}
        </span>
        <StatusBadge tone={c.spec.enabled ? status.tone : "neutral"}>{c.spec.enabled ? status.text : "Turned off"}</StatusBadge>
        {where !== "Local, code stays here" ? <span className="text-muted-foreground">{where}</span> : null}
        <span className="ml-auto">{c.fromFile ? <ManagedBy by="server file" reason="Set in forecrew.yaml; change it there" /> : allowedText(c, authorizations, local)}</span>
      </div>
      {c.spec.enabled && keyAdvice(check) ? <p className="mt-2 text-sm">{keyAdvice(check)}</p> : null}
      <details className="mt-2 text-sm">
        <summary className="cursor-pointer text-muted-foreground">Models and options</summary>
        <div className="space-y-3 pt-2">
          {models.length === 0 ? <p className="text-muted-foreground">No models found yet. They show once the key check lists them.</p> : null}
          <ul aria-label={`Models of ${c.name}`} className="space-y-2">
            {models.map((m) => {
              const t = tests.find((x) => x.model === m)
              const level = levelText(t)
              return (
                <li key={m} className="flex flex-wrap items-center gap-2">
                  <span className="font-mono">{m}</span>
                  <StatusBadge tone={level.tone}>{level.text}</StatusBadge>
                  {t?.stale ? <span className="text-muted-foreground">The agent changed: tested again soon</span> : null}
                  {t?.test?.error ? (
                    <details className="w-full text-xs text-muted-foreground">
                      <summary className="cursor-pointer">What the agent said (not verified)</summary>
                      <p className="whitespace-pre-wrap break-words">{t.test.error}</p>
                    </details>
                  ) : null}
                  {c.spec.enabled && !c.fromFile ? <TestModel connection={c.name} model={m} label={t ? "Test again" : "Test"} /> : null}
                </li>
              )
            })}
          </ul>
          <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
            <dt className="text-muted-foreground">Address</dt>
            <dd className="font-mono break-all">{c.spec.base_url || provider?.base_url || "the provider's own"}</dd>
            <dt className="text-muted-foreground">Agent</dt>
            <dd>{c.spec.adapter}</dd>
            {c.spec.key_env ? (
              <>
                <dt className="text-muted-foreground">Key variable</dt>
                <dd className="font-mono">{c.spec.key_env}</dd>
              </>
            ) : null}
            <dt className="text-muted-foreground">Key</dt>
            <dd>{c.spec.secret_ref ? "In secure storage, never shown" : local ? "None needed" : c.fromFile ? "In the server's environment" : "None"}</dd>
          </dl>
          {manage ? (
            <div className="flex flex-wrap gap-2">
              {c.spec.enabled ? (
                <Button variant="outline" size="sm" disabled={checking} onClick={() => void recheck()}>
                  Check the key again
                </Button>
              ) : null}
              <Button variant="outline" size="sm" disabled={draft.busy} onClick={() => void change(!c.spec.enabled)}>
                {c.spec.enabled ? "Turn off" : "Turn on"}
              </Button>
              <Button
                variant="link"
                size="sm"
                className="text-destructive"
                disabled={draft.busy}
                onClick={() => void draft.make(`Remove AI provider ${title}`, [{ kind: "model_connection", name: c.name, delete: true }])}
              >
                Remove…
              </Button>
            </div>
          ) : !c.fromFile ? (
            <PermButton permission="models.manage" variant="outline" size="sm">
              Change
            </PermButton>
          ) : null}
          {error ? (
            <p role="alert" className="text-destructive">
              {error}
            </p>
          ) : null}
        </div>
      </details>
    </li>
  )
}
