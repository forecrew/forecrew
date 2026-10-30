import { Link } from "@tanstack/react-router"
import { Card, CardContent, CardHeader } from "@/components/ui/card"

interface Props {
  title: string
  to: string
  search?: Record<string, string>
  linkLabel: string
  children: React.ReactNode
}

/** A summary card that opens the matching list. */
export function Tile({ title, to, search, linkLabel, children }: Props) {
  return (
    <Card>
      <CardHeader>
        <h2 className="text-sm font-medium text-label">{title}</h2>
      </CardHeader>
      <CardContent className="space-y-3">
        {children}
        <Link to={to} search={search as never} className="inline-flex min-h-11 items-center text-sm font-medium text-primary underline underline-offset-2">
          {linkLabel}
        </Link>
      </CardContent>
    </Card>
  )
}
