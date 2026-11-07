import { useQuery } from "@tanstack/react-query"
import { useSearch } from "@tanstack/react-router"
import { api } from "@/api/client"
import { keys, useGates, usePlans } from "@/api/queries"
import { Async } from "@/components/Async"
import { GateList } from "@/components/GateList"
import { PageHeader } from "@/components/PageHeader"
import { PlanCard } from "@/components/PlanCard"
import { StatusBadge } from "@/components/StatusBadge"

/** Plans waiting for the person: from people who may not confirm them, and from MCP clients, which never confirm in the client. */
function Setup({ open }: { open?: string }) {
  const plans = usePlans(true)
  const linked = useQuery({ queryKey: keys.plan(open ?? ""), queryFn: () => api.getPlan(open!), enabled: Boolean(open) })
  const items = [...(linked.data && !(plans.data ?? []).some((p) => p.id === linked.data.id) ? [linked.data] : []), ...(plans.data ?? [])]
  if (items.length === 0) return null
  return (
    <section aria-label="Setup" className="mb-6 space-y-3">
      <h2 className="flex items-center gap-2 text-lg font-semibold">
        <StatusBadge>Setup</StatusBadge> Plans waiting for you
      </h2>
      {items.map((p) => (
        <PlanCard key={p.id} plan={p} cancelLabel="Reject" />
      ))}
    </section>
  )
}

/** Pending gates: a list with filters, and the selected decision beside it. The project comes from the sidebar switcher. */
export function InboxPage() {
  const { project = "", gate, plan } = useSearch({ from: "/app/inbox" }) as { project?: string; gate?: string; plan?: string }
  const gates = useGates(project)
  const plans = usePlans(true)
  const waiting = (plans.data?.length ?? 0) > 0 || Boolean(plan)
  return (
    <>
      <PageHeader title="Inbox" />
      <Setup open={plan} />
      <Async query={gates} isEmpty={(d) => d.length === 0 && !waiting} empty={project ? "Nothing needs you in this project." : "Nothing needs you. New decisions show up here."}>
        {(items) => (items.length > 0 ? <GateList gates={items} keyboard initialRef={gate} /> : null)}
      </Async>
    </>
  )
}
