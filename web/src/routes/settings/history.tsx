import { useState } from "react"
import { useAudit, useMembers, useServiceAccounts } from "@/api/queries"
import type { AuditEntry, AuditFilter } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { Field } from "@/components/Field"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Section } from "./layout"

/** "member.suspended" as "Member suspended". */
export function actionText(action: string): string {
  const text = action.replaceAll(/[._]/g, " ")
  return text[0]!.toUpperCase() + text.slice(1)
}

const who = (e: AuditEntry) => e.actor_name || e.actor

/** A recorded value as text; it is data written by whoever acted, never markup. */
const show = (v: unknown) => (typeof v === "string" ? v : JSON.stringify(v))

function Details({ entry, onClose }: { entry?: AuditEntry; onClose: () => void }) {
  return (
    <Sheet open={Boolean(entry)} onOpenChange={(open) => !open && onClose()}>
      <SheetContent className="overflow-y-auto p-4">
        {entry ? (
          <>
            <SheetHeader className="p-0">
              <SheetTitle>{actionText(entry.action)}</SheetTitle>
              <SheetDescription>
                {who(entry)} · <time dateTime={entry.created_at}>{new Date(entry.created_at).toLocaleString()}</time>
              </SheetDescription>
            </SheetHeader>
            <dl className="mt-4 space-y-2 text-sm">
              <div>
                <dt className="text-muted-foreground">Actor</dt>
                <dd className="font-mono break-all">{entry.actor}</dd>
              </div>
              {entry.credential ? (
                <div>
                  <dt className="text-muted-foreground">Through</dt>
                  <dd className="font-mono break-all">{entry.credential}</dd>
                </div>
              ) : null}
              <div>
                <dt className="text-muted-foreground">What it was done to</dt>
                <dd className="font-mono break-all">
                  {entry.subject_type} {entry.subject_id}
                </dd>
              </div>
              {Object.entries(entry.data ?? {}).map(([k, v]) => (
                <div key={k}>
                  <dt className="text-muted-foreground">{k}</dt>
                  <dd className="break-words whitespace-pre-wrap">{show(v)}</dd>
                </div>
              ))}
            </dl>
          </>
        ) : null}
      </SheetContent>
    </Sheet>
  )
}

/** The audit log, shown as Change history: who did what, when, with filters. */
export function HistoryPage() {
  const [actor, setActor] = useState("")
  const [action, setAction] = useState("")
  const [type, setType] = useState("")
  const [since, setSince] = useState("")
  const [until, setUntil] = useState("")
  const [open, setOpen] = useState<AuditEntry>()
  const members = useMembers()
  const accounts = useServiceAccounts()
  const filter: AuditFilter = {
    actor, action: action.trim(), subject_type: type.trim(),
    since: since ? `${since}T00:00:00Z` : "", until: until ? `${until}T23:59:59Z` : "",
  }
  const entries = useAudit(filter)
  const clear = () => {
    setActor("")
    setAction("")
    setType("")
    setSince("")
    setUntil("")
  }

  return (
    <Section permission="audit.read" title="Change history" meta="Everything changed here, by anyone or anything, with who and when.">
      <form className="mb-4 flex flex-wrap items-end gap-3" aria-label="Filters" onSubmit={(e) => e.preventDefault()}>
        <Field label="Who">
          {(p) => (
            <NativeSelect {...p} value={actor} onChange={(e) => setActor(e.target.value)}>
              <NativeSelectOption value="">Anyone</NativeSelectOption>
              {(members.data ?? []).map((m) => (
                <NativeSelectOption key={m.user_id} value={`user:${m.user_id}`}>
                  {m.name}
                </NativeSelectOption>
              ))}
              {(accounts.data ?? []).map((a) => (
                <NativeSelectOption key={a.id} value={`service_account:${a.id}`}>
                  {a.name} (service account)
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </Field>
        <Field label="Action" hint="For example member.removed">
          {(p) => <Input {...p} className="w-44 font-mono" value={action} onChange={(e) => setAction(e.target.value)} />}
        </Field>
        <Field label="Done to" hint="For example team">
          {(p) => <Input {...p} className="w-32 font-mono" value={type} onChange={(e) => setType(e.target.value)} />}
        </Field>
        <Field label="From">{(p) => <Input {...p} type="date" value={since} onChange={(e) => setSince(e.target.value)} />}</Field>
        <Field label="To">{(p) => <Input {...p} type="date" value={until} onChange={(e) => setUntil(e.target.value)} />}</Field>
        <Button type="button" variant="outline" onClick={clear}>
          Clear filters
        </Button>
      </form>
      <Async query={entries} isEmpty={(d) => d.length === 0} empty="No changes yet. Everything changed here, by anyone or anything, will be listed.">
        {(items) => (
          <TableWrap label="Change history">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>When</TableHead>
                  <TableHead>What</TableHead>
                  <TableHead>Who</TableHead>
                  <TableHead>Done to</TableHead>
                  <TableHead>Details</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((e) => (
                  <TableRow key={e.id}>
                    <TableCell>
                      <Age iso={e.created_at} ago />
                    </TableCell>
                    <TableCell>{actionText(e.action)}</TableCell>
                    <TableCell>{who(e)}</TableCell>
                    <TableCell>
                      <span className="font-mono text-sm">{e.subject_type}</span>
                    </TableCell>
                    <TableCell>
                      <Button variant="ghost" aria-label={`Details of ${actionText(e.action)} ${e.id}`} onClick={() => setOpen(e)}>
                        Details
                      </Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        )}
      </Async>
      <Details entry={open} onClose={() => setOpen(undefined)} />
    </Section>
  )
}
