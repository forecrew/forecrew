import { useId } from "react"
import { Label } from "@/components/ui/label"

interface Props {
  label: string
  hint?: string
  error?: string
  children: (props: { id: string; "aria-describedby": string | undefined; "aria-invalid": boolean }) => React.ReactNode
}

/** A labelled form control with its hint and error linked for screen readers. */
export function Field({ label, hint, error, children }: Props) {
  const id = useId()
  const describedBy = [hint ? `${id}-hint` : "", error ? `${id}-error` : ""].filter(Boolean).join(" ") || undefined
  return (
    <div className="space-y-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children({ id, "aria-describedby": describedBy, "aria-invalid": Boolean(error) })}
      {hint ? (
        <p id={`${id}-hint`} className="text-xs text-label">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={`${id}-error`} className="text-sm text-danger">
          {error}
        </p>
      ) : null}
    </div>
  )
}
