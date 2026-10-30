import { useSearch } from "@tanstack/react-router"
import { useGates } from "@/api/queries"
import { Async } from "@/components/Async"
import { GateList } from "@/components/GateList"
import { PageHeader } from "@/components/PageHeader"

/** Pending gates: a list with filters, and the selected decision beside it. The project comes from the sidebar switcher. */
export function InboxPage() {
  const { project = "", gate } = useSearch({ from: "/app/inbox" })
  const gates = useGates(project)
  return (
    <>
      <PageHeader title="Inbox" />
      <Async query={gates} isEmpty={(d) => d.length === 0} empty={project ? "Nothing needs you in this project." : "Nothing needs you. New decisions show up here."}>
        {(items) => <GateList gates={items} keyboard initialRef={gate} />}
      </Async>
    </>
  )
}
