import { useNavigate, useSearch } from "@tanstack/react-router"
import { useGates } from "@/api/queries"
import { Async } from "@/components/Async"
import { GateList } from "@/components/GateList"
import { PageHeader } from "@/components/PageHeader"
import { ProjectSelect } from "@/components/ProjectSelect"
import { Label } from "@/components/ui/label"

/** Pending gates as decision cards. J and K move, A approves, R requests changes. */
export function InboxPage() {
  const { project = "" } = useSearch({ from: "/app/inbox" })
  const navigate = useNavigate({ from: "/inbox" })
  const gates = useGates(project)
  return (
    <>
      <PageHeader title="Inbox" />
      <div className="mb-4 space-y-1.5">
        <Label htmlFor="inbox-project">Project</Label>
        <ProjectSelect
          id="inbox-project"
          allLabel="All projects"
          value={project}
          onChange={(v) => void navigate({ search: (prev) => ({ ...prev, project: v || undefined }) })}
        />
      </div>
      <p className="mb-4 text-sm text-muted-foreground">
        Keyboard: <kbd className="font-mono">J</kbd> and <kbd className="font-mono">K</kbd> move between cards, <kbd className="font-mono">A</kbd> approves,{" "}
        <kbd className="font-mono">R</kbd> requests changes.
      </p>
      <Async query={gates} isEmpty={(d) => d.length === 0} empty={project ? "Nothing needs you in this project." : "Nothing needs you. New decisions show up here."}>
        {(items) => <GateList gates={items} keyboard />}
      </Async>
    </>
  )
}
