import { useCancelJob } from "@/api/queries"
import type { JobStatus } from "@/api/types"
import { ConfirmDialog } from "@/components/ConfirmDialog"

/** Only a queued or running job can be canceled. */
export const cancelable = (status: JobStatus) => status === "queued" || status === "running"

interface Props {
  /** The job ref, JOB-<number>. */
  jobRef: string
  /** The text of the button; table rows use a short one. */
  label?: string
  /** What screen readers announce; table rows name the job. */
  ariaLabel?: string
}

/** A "Cancel job" button that asks for confirmation first. */
export function CancelJob({ jobRef, label = "Cancel job", ariaLabel = "Cancel job" }: Props) {
  return (
    <ConfirmDialog
      trigger={label}
      triggerAriaLabel={ariaLabel}
      title={`Cancel ${jobRef}?`}
      description="A running job stops on its worker within about 30 seconds, and its playbook takes its failure path. A queued job is canceled at once."
      confirmLabel={`Cancel ${jobRef}`}
      keepLabel="Keep job"
      mutation={useCancelJob()}
      variables={jobRef}
    />
  )
}
