import { Link } from "@tanstack/react-router"
import { cn } from "cn"

/** A link to the trace of a ref (goal, task, assumption, ...): why does it exist? */
export function WhyLink({ to, className }: { to: string; className?: string }) {
  return (
    <Link to="/trace/$ref" params={{ ref: to }} aria-label={`Why ${to}?`} className={cn("text-sm text-primary underline underline-offset-2", className)}>
      Why?
    </Link>
  )
}
