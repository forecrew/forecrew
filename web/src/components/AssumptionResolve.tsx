import { useState } from "react"
import { useResolveAssumption } from "@/api/queries"
import type { AssumptionStatus } from "@/api/types"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"

type Resolution = "verified" | "accepted" | "invalidated"

const ACTIONS: { status: Resolution; label: string }[] = [
  { status: "verified", label: "Verify" },
  { status: "accepted", label: "Accept" },
  { status: "invalidated", label: "Invalidate" },
]

/** What a human may do to an assumption in each state (the domain's transitions). */
const NEXT: Record<AssumptionStatus, Resolution[]> = {
  unverified: ["verified", "accepted", "invalidated"],
  accepted: ["verified", "invalidated"],
  verified: ["invalidated"],
  invalidated: [],
}

/** Verify, accept or invalidate an assumption in place; the first and last need their evidence. */
export function AssumptionResolve({ assumption }: { assumption: { ref: string; status: AssumptionStatus } }) {
  const resolve = useResolveAssumption()
  const [choice, setChoice] = useState<Resolution | null>(null)
  const [evidence, setEvidence] = useState("")
  const [error, setError] = useState("")
  const allowed = ACTIONS.filter((a) => NEXT[assumption.status].includes(a.status))
  if (allowed.length === 0) return null

  const close = () => {
    setChoice(null)
    setEvidence("")
    setError("")
    resolve.reset()
  }
  const send = () => {
    if (!choice) return
    if (choice !== "accepted" && !evidence.trim()) {
      setError("Evidence is required to verify or invalidate.")
      return
    }
    setError("")
    resolve.mutate({ ref: assumption.ref, status: choice, evidence: evidence.trim() || undefined }, { onSuccess: close })
  }

  if (!choice) {
    return (
      <div className="flex flex-wrap gap-2" role="group" aria-label={`Resolve ${assumption.ref}`}>
        {allowed.map((a) => (
          <Button key={a.status} size="sm" variant="outline" aria-label={`${a.label} ${assumption.ref}`} onClick={() => setChoice(a.status)}>
            {a.label}
          </Button>
        ))}
      </div>
    )
  }
  const label = ACTIONS.find((a) => a.status === choice)!.label
  return (
    <div className="space-y-2">
      <Field
        label={`Evidence for ${assumption.ref}${choice === "accepted" ? " (optional)" : ""}`}
        hint={choice === "invalidated" ? "Invalidating sends unmerged dependent tasks back and opens a goal to re-evaluate merged work." : undefined}
        error={error}
      >
        {(p) => <Textarea {...p} rows={3} value={evidence} onChange={(e) => setEvidence(e.target.value)} required={choice !== "accepted"} />}
      </Field>
      {resolve.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {resolve.error.message}
        </p>
      ) : null}
      <div className="flex flex-wrap gap-2">
        <Button size="sm" variant={choice === "invalidated" ? "destructive" : "default"} onClick={send} disabled={resolve.isPending}>
          {`Confirm ${label.toLowerCase()}`}
        </Button>
        <Button size="sm" variant="ghost" onClick={close}>
          Cancel
        </Button>
      </div>
    </div>
  )
}
