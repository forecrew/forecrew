import { CheckIcon, CopyIcon } from "lucide-react"
import { useState } from "react"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"

/**
 * A secret the server shows only once (a token, an invitation link) with a
 * Copy button. It is plain text and leaves the page when it is dismissed.
 */
export function ShowOnce({ title, secret, label, onDismiss }: { title: string; secret: string; label: string; onDismiss: () => void }) {
  const [copied, setCopied] = useState(false)
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(secret)
      setCopied(true)
    } catch {
      setCopied(false) // the text stays selectable
    }
  }
  return (
    <Card role="region" aria-label={title}>
      <CardHeader>
        <h2 className="font-medium">{title}</h2>
        <p className="text-sm text-muted-foreground">It is shown only now. Copy it and keep it safe: it cannot be shown again.</p>
      </CardHeader>
      <CardContent className="space-y-3">
        <label className="block space-y-1.5 text-sm">
          <span className="font-medium">{label}</span>
          <input readOnly value={secret} onFocus={(e) => e.currentTarget.select()} className="block h-8 w-full rounded-lg border border-input bg-transparent px-2.5 font-mono text-sm" />
        </label>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" onClick={() => void copy()}>
            {copied ? <CheckIcon aria-hidden="true" /> : <CopyIcon aria-hidden="true" />}
            {copied ? "Copied" : "Copy"}
          </Button>
          <Button variant="ghost" onClick={onDismiss}>
            Done
          </Button>
        </div>
      </CardContent>
    </Card>
  )
}
