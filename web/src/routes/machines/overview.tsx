import { Link } from "@tanstack/react-router"
import type { FleetLocation, FleetOverview, FleetRouting, LocationStatus } from "@/api/types"
import { AskButton } from "@/components/assistant/AskButton"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Card, CardContent } from "@/components/ui/card"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import type { Tone } from "@/lib/format"
import { WaitFix } from "./fix"
import { AddCards } from "./add-cards"

const STATUS: Record<LocationStatus, { text: string; tone: Tone }> = {
  online: { text: "Online", tone: "good" },
  offline: { text: "Offline", tone: "bad" },
  paused: { text: "Paused", tone: "attention" },
  pending: { text: "Waiting to connect", tone: "attention" },
  revoked: { text: "Removed", tone: "neutral" },
}

const ROUTING: Record<FleetRouting, string> = {
  own_only: "Jobs run on your machines only",
  own_first: "Jobs run on your machines first, then Forecrew Cloud",
  hosted: "Jobs run on Forecrew Cloud",
}

function Tile({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Card size="sm">
      <CardContent>
        <p className="text-sm text-muted-foreground">{label}</p>
        <p className="text-xl font-semibold">{children}</p>
      </CardContent>
    </Card>
  )
}

const inUse = (l: FleetLocation) => (l.kind === "computer" ? "" : l.capacity > 0 ? `${l.in_use} of ${l.capacity}` : String(l.in_use))

/** The overview: whether jobs have somewhere to run, and where. Waiting and Running hide when empty. */
export function Overview({ fleet }: { fleet: FleetOverview }) {
  if (fleet.locations.length === 0 && fleet.queue.length === 0) {
    return (
      <section aria-label="No machines yet" className="space-y-4 rounded-xl border border-dashed bg-card p-6">
        <p className="text-center text-muted-foreground">No machines yet. Agents need somewhere to work.</p>
        <AddCards />
        <p className="flex items-center justify-center gap-2 text-sm text-muted-foreground">
          Or just say what you have. <AskButton message="I have a computer I want to run jobs on" label="Ask the assistant" />
        </p>
      </section>
    )
  }
  return (
    <div className="space-y-6">
      <div className="grid gap-3 sm:grid-cols-3">
        <Tile label="Working now">{fleet.working} {fleet.working === 1 ? "machine" : "machines"}</Tile>
        <Tile label="Waiting">{fleet.waiting} {fleet.waiting === 1 ? "job" : "jobs"}</Tile>
        {fleet.spend ? (
          <Tile label="Spend this month">
            {fleet.spend.currency} {Math.round(fleet.spend.amount)}
            {fleet.spend.limit ? ` of ${Math.round(fleet.spend.limit)}` : ""}
          </Tile>
        ) : null}
      </div>
      <section aria-labelledby="locations" className="space-y-2">
        <h2 id="locations" className="text-lg font-semibold">
          Locations
        </h2>
        <TableWrap label="Locations">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Location</TableHead>
                <TableHead>Runs</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>In use</TableHead>
                <TableHead>Cost</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {fleet.locations.map((l) => {
                const s = STATUS[l.status]
                return (
                  <TableRow key={l.id}>
                    <TableCell className="font-medium">
                      {l.kind === "location" ? (
                        <Link to="/machines/$id" params={{ id: l.id }} className="underline underline-offset-2">
                          {l.name}
                        </Link>
                      ) : (
                        l.name
                      )}
                      {l.problems?.map((p) => (
                        <p key={p} className="mt-1 text-sm font-normal text-destructive">
                          {p}
                        </p>
                      ))}
                    </TableCell>
                    <TableCell>{l.runs}</TableCell>
                    <TableCell>
                      <StatusBadge tone={s.tone}>{s.text}</StatusBadge>
                    </TableCell>
                    <TableCell>{inUse(l)}</TableCell>
                    <TableCell>{l.cost}</TableCell>
                  </TableRow>
                )
              })}
            </TableBody>
          </Table>
        </TableWrap>
      </section>
      {fleet.queue.length > 0 ? (
        <section aria-labelledby="waiting" className="space-y-2">
          <h2 id="waiting" className="text-lg font-semibold">
            Waiting
          </h2>
          <TableWrap label="Waiting jobs">
            <Table>
              <TableBody>
                {fleet.queue.map((j) => (
                  <TableRow key={j.ref}>
                    <TableCell>
                      <Link to="/jobs/$ref" params={{ ref: j.ref }} className="font-mono underline underline-offset-2">
                        {j.ref}
                      </Link>
                    </TableCell>
                    <TableCell>{j.role}</TableCell>
                    <TableCell>{j.sentence}.</TableCell>
                    <TableCell>
                      <WaitFix code={j.code} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        </section>
      ) : null}
      {fleet.running.length > 0 ? (
        <section aria-labelledby="running" className="space-y-2">
          <h2 id="running" className="text-lg font-semibold">
            Running now
          </h2>
          <TableWrap label="Running jobs">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Machine</TableHead>
                  <TableHead>Location</TableHead>
                  <TableHead>Job</TableHead>
                  <TableHead>For</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {fleet.running.map((r) => (
                  <TableRow key={r.ref}>
                    <TableCell className="font-mono">{r.machine}</TableCell>
                    <TableCell>{r.location}</TableCell>
                    <TableCell>
                      <Link to="/jobs/$ref" params={{ ref: r.ref }} className="underline underline-offset-2">
                        {r.ref} {r.role}
                      </Link>
                    </TableCell>
                    <TableCell>{r.minutes} min</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        </section>
      ) : null}
      <p className="text-sm text-muted-foreground">
        {ROUTING[fleet.routing]} ·{" "}
        <Link to="/settings/policies" className="underline underline-offset-2">
          Change
        </Link>
      </p>
    </div>
  )
}
