import { Badge } from "@/components/ui/badge"
import { cn } from "cn"
import type { Tone } from "@/lib/format"

const TONES: Record<Tone, string> = {
  good: "border-success/50 text-success",
  attention: "border-warning/50 text-warning",
  bad: "border-danger/50 text-destructive",
  neutral: "text-muted-foreground",
}

/** A status in words; the color only adds to the text. */
export function StatusBadge({ tone = "neutral", children, className }: { tone?: Tone; children: React.ReactNode; className?: string }) {
  return (
    <Badge variant="outline" className={cn("whitespace-nowrap bg-card", TONES[tone], className)}>
      {children}
    </Badge>
  )
}
