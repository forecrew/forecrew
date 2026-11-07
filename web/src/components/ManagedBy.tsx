import { StatusBadge } from "@/components/StatusBadge"

/** The one badge for a value Forecrew may not change here, with the reason (a server file, Okta, a repo file). */
export function ManagedBy({ by, reason }: { by: string; reason?: string }) {
  return (
    <StatusBadge>
      <span title={reason}>Managed by {by}</span>
    </StatusBadge>
  )
}
