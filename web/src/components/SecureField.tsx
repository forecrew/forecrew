import { CheckIcon, LockIcon } from "lucide-react"
import { useId, useState } from "react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"

/**
 * A secret goes straight to secure storage: a password input outside any
 * message bubble, and afterwards only "Saved". The server decides when one is
 * drawn (a secret_required step); the value is never shown or read back.
 */
export function SecureField({ label, filled, reuses, onSave }: { label: string; filled: boolean; reuses?: boolean; onSave: (value: string) => Promise<unknown> }) {
  const id = useId()
  const [value, setValue] = useState("")
  const [error, setError] = useState("")
  const [saving, setSaving] = useState(false)
  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!value) return
    setSaving(true)
    setError("")
    try {
      await onSave(value)
      setValue("") // nothing typed stays in the page
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not save it")
    } finally {
      setSaving(false)
    }
  }
  return (
    <form onSubmit={(e) => void save(e)} aria-label={`Secure field: ${label}`} className="space-y-1.5 rounded-lg border bg-muted/40 p-3">
      <p className="flex items-center gap-1.5 text-xs font-medium tracking-wide text-muted-foreground uppercase">
        <LockIcon className="size-3.5" aria-hidden="true" /> Secure field
      </p>
      {filled ? (
        <p className="flex items-center gap-1.5 text-sm font-medium">
          <CheckIcon className="size-4 text-success" aria-hidden="true" /> {label}: {reuses ? "Saved before, it is used as it is" : "Saved"}
        </p>
      ) : (
        <>
          <label className="block text-sm font-medium" htmlFor={id}>
            {label}
          </label>
          <div className="flex items-center gap-2">
            <Input id={id} type="password" autoComplete="off" spellCheck={false} className="font-mono" value={value} onChange={(e) => setValue(e.target.value)} />
            <Button type="submit" disabled={saving || !value}>
              Save
            </Button>
          </div>
        </>
      )}
      <p className="text-xs text-muted-foreground">Goes straight to secure storage. The assistant and the change history never see it.</p>
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
    </form>
  )
}
