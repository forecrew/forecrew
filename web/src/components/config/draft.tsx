import { useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { api } from "@/api/client"
import { CONFIG_KEYS } from "@/api/queries"
import type { ConfigPlan, ConfigWanted } from "@/api/types"
import { PlanCard } from "@/components/PlanCard"
import { Button } from "@/components/ui/button"

/**
 * The plan a form makes: Save never applies by itself. The form asks for a plan, the plan card
 * shows what would change, and its button confirms. Edit goes back to the form.
 */
export function useDraft() {
  const [plan, setPlan] = useState<ConfigPlan>()
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const qc = useQueryClient()
  return {
    plan,
    error,
    busy,
    /** Asks the server for a plan; the form shows why when it is refused. */
    async make(title: string, changes: ConfigWanted[], request?: string) {
      setBusy(true)
      setError("")
      try {
        setPlan(await api.planConfig(title, changes, request))
        return true
      } catch (err) {
        setError(err instanceof Error ? err.message : "Could not make the plan")
        return false
      } finally {
        setBusy(false)
      }
    },
    /** Drops the plan (and what it staged) and goes back to the form. */
    async discard() {
      const old = plan
      setPlan(undefined)
      setError("")
      if (old?.status === "pending" || old?.status === "awaiting_approval") await api.cancelPlan(old.id).catch(() => {})
      await Promise.all(CONFIG_KEYS.map((queryKey) => qc.invalidateQueries({ queryKey })))
    },
    setPlan,
  }
}

/** The card of a draft, with Edit back to the form and a way to start another change after it is done. */
export function DraftPlan({ draft, onEdit, onDone }: { draft: ReturnType<typeof useDraft>; onEdit: () => void; onDone?: () => void }) {
  if (!draft.plan) return null
  return (
    <div className="space-y-2">
      <PlanCard plan={draft.plan} onChange={draft.setPlan} onEdit={onEdit} onRebuilt={draft.setPlan} />
      {draft.plan.status === "applied" && onDone ? (
        <Button variant="outline" onClick={onDone}>
          Done
        </Button>
      ) : null}
    </div>
  )
}
