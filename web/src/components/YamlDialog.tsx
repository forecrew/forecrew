import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog"

/** A link-like button that shows resources as YAML in a dialog, read-only. */
export function YamlDialog({ title, yaml, label = "View as YAML" }: { title: string; yaml: string; label?: string }) {
  return (
    <Dialog>
      <DialogTrigger render={<Button variant="link" className="h-auto p-0" />}>{label}</DialogTrigger>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          <DialogDescription>The resources, as the schema names them. Secrets are never part of them.</DialogDescription>
        </DialogHeader>
        <pre tabIndex={0} className="max-h-96 overflow-auto rounded-lg bg-muted p-3 font-mono text-xs">
          {yaml}
        </pre>
      </DialogContent>
    </Dialog>
  )
}
