import { ExternalLinkIcon } from "lucide-react"
import { cn } from "cn"

/** A link to GitHub (or another site), which opens in a new tab. */
export function ExternalLink({ href, children, className }: { href: string; children: React.ReactNode; className?: string }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className={cn("inline-flex items-center gap-1 text-primary underline underline-offset-2", className)}
    >
      {children}
      <ExternalLinkIcon className="size-3.5" aria-hidden="true" />
      <span className="sr-only">{" "}(opens in a new tab)</span>
    </a>
  )
}
