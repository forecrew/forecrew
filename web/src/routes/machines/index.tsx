import { useNavigate, useSearch } from "@tanstack/react-router"
import { useFleet } from "@/api/queries"
import { AskButton } from "@/components/assistant/AskButton"
import { Async } from "@/components/Async"
import { PageHeader } from "@/components/PageHeader"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useCan } from "@/lib/permissions"
import { AddMachinesLink } from "./add-link"
import { Overview } from "./overview"
import { TypesTab } from "./types"

/** Machines: see in five seconds whether jobs have somewhere to run, then dig into where. */
export function MachinesPage() {
  const fleet = useFleet()
  const can = useCan()
  const navigate = useNavigate()
  const { tab } = useSearch({ strict: false }) as { tab?: string }
  const current = tab === "types" ? "types" : "overview"
  return (
    <>
      <PageHeader title="Machines" meta={fleet.data && (fleet.data.locations.length > 0 || fleet.data.queue.length > 0) ? fleet.data.headline : undefined}>
        <AskButton />
        {can("worker.manage") ? <AddMachinesLink /> : null}
      </PageHeader>
      <Tabs value={current} onValueChange={(v) => void navigate({ to: "/machines", search: (v === "types" ? { tab: "types" } : {}) as never })}>
        <TabsList aria-label="Machines">
          <TabsTrigger value="overview">Overview</TabsTrigger>
          <TabsTrigger value="types">Machine types</TabsTrigger>
        </TabsList>
      </Tabs>
      <div className="mt-4">
        {current === "types" ? <TypesTab /> : <Async query={fleet}>{(d) => <Overview fleet={d} />}</Async>}
      </div>
    </>
  )
}
