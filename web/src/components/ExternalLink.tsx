import { ExternalLinkIcon } from "lucide-react"

/** A link to GitHub (or another site), which opens in a new tab. */
export function ExternalLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <a
      href={href}
      target="_blank"
      rel="noopener noreferrer"
      className="inline-flex min-h-11 items-center gap-1 text-primary underline underline-offset-2"
    >
      {children}
      <ExternalLinkIcon className="size-3.5" aria-hidden="true" />
      <span className="sr-only">{" "}(opens in a new tab)</span>
    </a>
  )
}
