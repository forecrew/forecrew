import { Link } from "@tanstack/react-router"
import { useShapes } from "@/api/queries"
import { Card, CardContent } from "@/components/ui/card"

const CARDS = [
  { kind: "computer", title: "A computer or server", text: "Home server, office hypervisor, Mac, Windows PC.", cost: "Free." },
  { kind: "aws", title: "My AWS account", text: "Machines start in your account. Your code stays there.", cost: "AWS bills you." },
  { kind: "cloud", title: "Forecrew Cloud", text: "Ready now. Pay per minute. Nothing to run.", cost: "" },
] as const

/** What do you want to connect? Forecrew Cloud is only offered on a server with a hosted connection. */
export function AddCards() {
  // Forecrew Cloud exists where the server offers machine types of its own.
  const hosted = useShapes().data?.some((s) => s.ownership === "hosted") ?? false
  return (
    <ul aria-label="What do you want to connect?" className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
      {CARDS.filter((c) => c.kind !== "cloud" || hosted).map((c) => (
        <li key={c.kind}>
          <Link to="/machines/add" search={{ kind: c.kind } as never} className="block h-full rounded-xl focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-none">
            <Card className="h-full hover:bg-accent/50">
              <CardContent className="space-y-1">
                <p className="font-medium">{c.title}</p>
                <p className="text-sm text-muted-foreground">{c.text}</p>
                {c.cost ? <p className="text-sm">{c.cost}</p> : null}
              </CardContent>
            </Card>
          </Link>
        </li>
      ))}
    </ul>
  )
}
