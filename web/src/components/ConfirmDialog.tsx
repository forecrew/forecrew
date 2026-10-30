import type { UseMutationResult } from "@tanstack/react-query"
import { useState } from "react"
import { Button } from "@/components/ui/button"
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger,
} from "@/components/ui/dialog"

interface Props<T, V> {
  /** The visible text of the button that opens the dialog. */
  trigger: string
  /** What screen readers announce for that button (rows of a table name their item). */
  triggerAriaLabel: string
  title: string
  description: string
  /** An extra sentence in the alert color, for a consequence to be aware of. */
  warning?: string
  confirmLabel: string
  keepLabel: string
  mutation: UseMutationResult<T, Error, V>
  /** What the mutation is called with on confirm. */
  variables: V
  onSuccess?: (data: T) => void | Promise<void>
}

/**
 * A button that asks for confirmation in a dialog before it runs a mutation.
 * The dialog closes on success and shows the API's message on failure.
 */
export function ConfirmDialog<T, V>({
  trigger, triggerAriaLabel, title, description, warning, confirmLabel, keepLabel, mutation, variables, onSuccess,
}: Props<T, V>) {
  const [open, setOpen] = useState(false)
  const onOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) mutation.reset()
  }
  const confirm = () =>
    mutation.mutate(variables, {
      onSuccess: async (data) => {
        onOpenChange(false)
        await onSuccess?.(data)
      },
    })

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger asChild>
        <Button variant="outline" aria-label={triggerAriaLabel}>
          {trigger}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>{description}</DialogDescription>
        </DialogHeader>
        {warning ? (
          <p role="alert" className="text-sm font-medium text-danger">
            {warning}
          </p>
        ) : null}
        {mutation.isError ? (
          <p role="alert" className="text-sm text-danger">
            {mutation.error.message}
          </p>
        ) : null}
        <DialogFooter>
          <Button variant="outline" onClick={() => onOpenChange(false)}>
            {keepLabel}
          </Button>
          <Button variant="destructive" onClick={confirm} disabled={mutation.isPending}>
            {confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
