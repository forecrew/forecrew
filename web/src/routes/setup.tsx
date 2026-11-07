import { Link, useSearch } from "@tanstack/react-router"
import { useFleet, useProjects, useResources, useShapes } from "@/api/queries"
import { AskButton } from "@/components/assistant/AskButton"
import { PageHeader } from "@/components/PageHeader"
import { Card, CardContent } from "@/components/ui/card"
import { buttonVariants } from "@/components/ui/button"
import { useCan } from "@/lib/permissions"

type Step = "where" | "repo" | "goal"
const STEPS: { id: Step; title: string }[] = [
  { id: "where", title: "Where should agents work?" },
  { id: "repo", title: "Pick a repository" },
  { id: "goal", title: "Describe a first goal" },
]

function Choice({ to, search, title, text }: { to: string; search?: Record<string, string>; title: string; text: string }) {
  return (
    <li>
      <Link to={to} search={search as never} className="block h-full rounded-xl focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-none">
        <Card className="h-full hover:bg-accent/50">
          <CardContent className="space-y-1">
            <p className="font-medium">{title}</p>
            <p className="text-sm text-muted-foreground">{text}</p>
          </CardContent>
        </Card>
      </Link>
    </li>
  )
}

/** Agents need a model: with no key set, setup says so first and links to AI models. */
function ModelsFirst() {
  const can = useCan()
  const connections = useResources("model_connection", can("models.read"))
  if (!can("models.manage") || connections.data === undefined || connections.data.length > 0) return null
  return (
    <section aria-label="No AI provider yet" className="mb-6 max-w-3xl rounded-xl border bg-card p-4">
      <h2 className="font-medium">Add an AI provider key first</h2>
      <p className="mb-2 text-sm text-muted-foreground">Agents work with your own keys. One is enough: Forecrew finds its models and gives them to the roles.</p>
      <Link to="/settings/models" className={buttonVariants({ variant: "outline" })}>
        Add a key
      </Link>
    </section>
  )
}

/**
 * First-run setup: where jobs run comes before the repository, because onboarding is itself a
 * job and needs a machine. Each step names its decision; later ones follow the earlier.
 */
export function SetupPage() {
  const { tab } = useSearch({ strict: false }) as { tab?: string }
  const fleet = useFleet()
  const projects = useProjects()
  const shapes = useShapes()
  const hosted = shapes.data?.some((s) => s.ownership === "hosted") ?? false
  const hasMachines = (fleet.data?.locations.length ?? 0) > 0
  // The step the person is on: the earliest not done, unless they chose to decide later.
  const done: Record<Step, boolean> = { where: hasMachines || tab === "repo" || tab === "goal", repo: (projects.data?.length ?? 0) > 0 || tab === "goal", goal: false }
  const current = STEPS.find((s) => !done[s.id])!.id
  return (
    <>
      <ModelsFirst />
      <PageHeader title="Set up Forecrew" meta="Three decisions: where jobs run, which repository, the first goal.">
        <AskButton message="Help me set up Forecrew" />
      </PageHeader>
      <ol className="max-w-3xl space-y-6">
        {STEPS.map((s, i) => (
          <li key={s.id} aria-current={s.id === current ? "step" : undefined} className={s.id === current ? "" : "text-muted-foreground"}>
            <h2 className="mb-2 text-lg font-semibold">
              {i + 1}. {s.title} {done[s.id] ? <span className="text-sm font-normal">(done)</span> : null}
            </h2>
            {s.id === current && s.id === "where" ? (
              <ul aria-label="Where should agents work?" className="grid gap-3 sm:grid-cols-3">
                {hosted ? <Choice to="/machines" title="Forecrew Cloud" text="Ready now, starts at once. Pay per minute." /> : null}
                <Choice to="/machines/add" search={{ kind: "computer" }} title="A computer of mine" text="A server, a Mac or a PC. Free. Run one line, then confirm." />
                <Choice to="/setup" search={{ tab: "repo" }} title="Decide later" text="Onboarding needs a machine, so jobs will wait until you add one." />
              </ul>
            ) : null}
            {s.id === current && s.id === "repo" ? (
              <Link to="/projects/new" className={buttonVariants()}>
                Pick a repository
              </Link>
            ) : null}
            {s.id === current && s.id === "goal" ? (
              <Link to="/goals/new" className={buttonVariants()}>
                Describe a first goal
              </Link>
            ) : null}
          </li>
        ))}
      </ol>
    </>
  )
}
