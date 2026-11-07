import { Link } from "@tanstack/react-router"
import type { WaitReasonCode } from "@/api/types"
import { useAssistant } from "@/components/assistant/AssistantContext"
import { Button, buttonVariants } from "@/components/ui/button"
import { useCan } from "@/lib/permissions"

/** What fixes each reason a job waits for, as a link or a question to the assistant; nothing for what fixes itself. */
const FIXES: Partial<Record<WaitReasonCode, { label: string; to?: string; ask: string }>> = {
  location_offline: { label: "How to bring it back", ask: "Why is my location offline and how do I bring it back?" },
  no_machine_type: { label: "Add machines", to: "/machines/add", ask: "Add a machine type that can run this job" },
  machine_limit: { label: "Raise the limit", to: "/settings/policies", ask: "Raise the limit of machines at once" },
  budget: { label: "Raise the limit", to: "/settings/policies", ask: "Raise the monthly limit" },
  policy: { label: "Change policy", to: "/settings/policies", ask: "Let jobs use Forecrew Cloud when my machines are busy" },
  trust_zone: { label: "How to start one", ask: "How do I start a machine in the zone this job needs?" },
}

/**
 * The one fix button of a waiting job. Someone who may manage machines goes to the fix; a
 * person who may not gets Ask an admin, which starts the plan with the assistant for approval.
 */
export function WaitFix({ code }: { code: WaitReasonCode }) {
  const can = useCan()
  const { ask } = useAssistant()
  const fix = FIXES[code]
  if (!fix || !can("config.read")) return null
  // A viewer may not make plans: no fix, only the sentence.
  if (!can("config.apply")) return null
  const budget = code === "budget" || code === "machine_limit"
  const mayFix = can(budget ? "billing.manage" : "worker.manage")
  if (!mayFix) {
    return (
      <Button variant="outline" size="sm" onClick={() => ask(fix.ask)}>
        Ask an admin
      </Button>
    )
  }
  if (fix.to) {
    return (
      <Link to={fix.to} className={buttonVariants({ variant: "outline", size: "sm" })}>
        {fix.label}
      </Link>
    )
  }
  return (
    <Button variant="outline" size="sm" onClick={() => ask(fix.ask)}>
      {fix.label}
    </Button>
  )
}
