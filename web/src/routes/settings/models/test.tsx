import { useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { api } from "@/api/client"
import { keys } from "@/api/queries"
import type { ModelTest } from "@/api/types"
import { Button } from "@/components/ui/button"
import { cost } from "@/lib/format"
import { PermButton } from "@/lib/permissions"

/**
 * Tests a model on a connection, with its cost shown first: the first click asks for the
 * estimate, the second runs the test. The result comes with the key checks.
 */
export function TestModel({ connection, model, label = "Test" }: { connection: string; model: string; label?: string }) {
  const qc = useQueryClient()
  const [estimate, setEstimate] = useState<ModelTest>()
  const [started, setStarted] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState("")
  const call = async (run: boolean) => {
    setBusy(true)
    setError("")
    try {
      const out = await api.testModel(connection, model, run)
      if (run) {
        setStarted(true)
        setEstimate(undefined)
        await qc.invalidateQueries({ queryKey: keys.modelChecks })
      } else {
        setEstimate(out)
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "The test could not start")
    } finally {
      setBusy(false)
    }
  }
  if (started) return <span className="text-sm text-muted-foreground">Test started. The result shows here when it ends.</span>
  if (!estimate) {
    return (
      <span className="inline-flex flex-col gap-1">
        <PermButton permission="models.manage" variant="outline" size="sm" disabled={busy} aria-label={`${label} ${model} on ${connection}`} onClick={() => void call(false)}>
          {label}
        </PermButton>
        {error ? (
          <span role="alert" className="text-sm text-destructive">
            {error}
          </span>
        ) : null}
      </span>
    )
  }
  const e = estimate.estimate
  return (
    <div role="group" aria-label={`Test of ${model}`} className="space-y-2 rounded-lg border p-3 text-sm">
      <p>
        A test is a short job: at most {estimate.max_turns} turns and {Math.round(estimate.timeout_seconds / 60)} minutes. It costs about{" "}
        <span className="font-medium">{e.cost_usd === undefined ? "an unknown amount" : cost(e.cost_usd)}</span>
        {e.worst_cost_usd === undefined ? "" : `, at most ${cost(e.worst_cost_usd)}`}, paid to the provider.
      </p>
      <p className="text-xs text-muted-foreground">{e.cost_usd === undefined ? e.basis : `Typical: ${e.basis}. Worst case: ${e.worst_basis}.`}</p>
      <div className="flex gap-2">
        <Button size="sm" disabled={busy} onClick={() => void call(true)}>
          Run the test
        </Button>
        <Button size="sm" variant="ghost" onClick={() => setEstimate(undefined)}>
          Not now
        </Button>
      </div>
      {error ? (
        <p role="alert" className="text-destructive">
          {error}
        </p>
      ) : null}
    </div>
  )
}
