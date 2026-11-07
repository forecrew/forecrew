import { Link, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { AskButton } from "@/components/assistant/AskButton"
import { PageHeader } from "@/components/PageHeader"
import { Field } from "@/components/Field"
import { Input } from "@/components/ui/input"
import { useCan } from "@/lib/permissions"
import { AddCards } from "./add-cards"
import { JoinFlow } from "./join"

function Aws() {
  const [region, setRegion] = useState("eu-central-1")
  const [limit, setLimit] = useState("")
  const can = useCan()
  return (
    <div className="space-y-4">
      <div className="max-w-sm space-y-3">
        <Field label="Region">{(p) => <Input {...p} className="font-mono" value={region} onChange={(e) => setRegion(e.target.value)} />}</Field>
        <Field label="Monthly limit" hint={can("billing.manage") ? "Required: an unbounded cloud bill is not a default we accept." : "Required. Setting it needs Manage budgets, so the plan will ask for approval."}>
          {(p) => <Input {...p} inputMode="decimal" value={limit} onChange={(e) => setLimit(e.target.value)} />}
        </Field>
      </div>
      {Number(limit) > 0 ? <JoinFlow cloud budget={Number(limit)} limitText={`Region ${region}, at most ${limit} a month.`} /> : <p className="text-sm text-muted-foreground">Enter a monthly limit to continue.</p>}
    </div>
  )
}

/** Add machines: from "I have this" to a running location. The assistant drives the same flow. */
export function AddMachinesPage() {
  const { kind } = useSearch({ strict: false }) as { kind?: string }
  const can = useCan()
  return (
    <>
      <Link to="/machines" className="mb-2 inline-block text-sm underline underline-offset-2">
        ← Machines
      </Link>
      <PageHeader title="Add machines" meta="What do you want to connect?">
        <AskButton message="Help me add machines" />
      </PageHeader>
      {!can("worker.manage") ? (
        <p className="rounded-xl border border-dashed bg-card p-6 text-center text-muted-foreground">
          Adding machines needs Manage machines. Ask an admin, or tell the assistant what you have and it will send them the plan.
        </p>
      ) : kind === "computer" ? (
        <JoinFlow />
      ) : kind === "aws" ? (
        <Aws />
      ) : (
        <div className="space-y-4">
          <AddCards />
          <p className="text-sm text-muted-foreground">Only need tests on Windows or macOS? GitHub Actions already runs them, with nothing to install. It runs tests only, not agents.</p>
          <p className="text-sm text-muted-foreground">Not sure? Tell the assistant what you have.</p>
        </div>
      )}
    </>
  )
}
