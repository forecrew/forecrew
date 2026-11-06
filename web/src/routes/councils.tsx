import { Link, useNavigate, useParams, useSearch } from "@tanstack/react-router"
import { useState } from "react"
import { useAddConstraint, useCouncil, useCouncils, useProjects, useStartCouncil } from "@/api/queries"
import { COUNCIL_KINDS, COUNCIL_STAGES, type CouncilDetail, type CouncilStatus, type Contribution } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { BriefView } from "@/components/CouncilBrief"
import { ContributionCard } from "@/components/ContributionCard"
import { Field } from "@/components/Field"
import { PageHeader } from "@/components/PageHeader"
import { ProjectSelect } from "@/components/ProjectSelect"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button, buttonVariants } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"
import { councilTone, decisionRef, splitLines, words } from "@/lib/format"
import { cn } from "cn"

const STATUSES: CouncilStatus[] = ["running", "decided", "failed", "canceled"]
const DEVILS_ADVOCATE = "devils-advocate"

/** All councils, newest first. The project comes from the sidebar switcher. */
export function CouncilsPage() {
  const { project = "", status = "" } = useSearch({ from: "/app/councils" })
  const navigate = useNavigate({ from: "/councils" })
  const councils = useCouncils(project, status as CouncilStatus | "")
  const projects = useProjects()
  const names = new Map(projects.data?.map((p) => [p.id, p.name]))
  return (
    <>
      <PageHeader title="Councils">
        <Link to="/councils/new" search={project ? { project } : {}} className={cn(buttonVariants())}>
          Start a council
        </Link>
      </PageHeader>
      <div className="mb-4 space-y-1.5">
        <Label htmlFor="council-status">Status</Label>
        <NativeSelect
          id="council-status"
          className="w-full sm:w-48"
          value={status}
          onChange={(e) => void navigate({ search: (prev) => ({ ...prev, status: e.target.value || undefined }) })}
        >
          <NativeSelectOption value="">All statuses</NativeSelectOption>
          {STATUSES.map((s) => (
            <NativeSelectOption key={s} value={s}>
              {s}
            </NativeSelectOption>
          ))}
        </NativeSelect>
      </div>
      <Async
        query={councils}
        isEmpty={(d) => d.length === 0}
        empty={status ? `No ${status} councils${project ? " in this project" : ""}.` : "No councils yet. Start one with Start a council."}
      >
        {(items) => (
          <TableWrap label="Councils">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Council</TableHead>
                  <TableHead>Project</TableHead>
                  <TableHead>Kind</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Started</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((c) => (
                  <TableRow key={c.id}>
                    <TableCell className="whitespace-normal">
                      <Link to="/councils/$ref" params={{ ref: c.ref }} className="underline-offset-2 hover:underline">
                        <span className="font-mono">{c.ref}</span> {c.question}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">{names.get(c.project_id) ?? ""}</TableCell>
                    <TableCell>{words(c.kind)}</TableCell>
                    <TableCell>
                      <StatusBadge tone={councilTone(c.status)}>{c.status}</StatusBadge>
                    </TableCell>
                    <TableCell>
                      <Age iso={c.created_at} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        )}
      </Async>
    </>
  )
}

/** Adds a constraint the next round of a running council sees. */
function ConstraintForm({ council }: { council: CouncilDetail }) {
  const add = useAddConstraint(council.ref)
  const [text, setText] = useState("")
  const [error, setError] = useState("")
  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!text.trim()) {
      setError("Write the constraint first.")
      return
    }
    setError("")
    add.mutate(text.trim(), { onSuccess: () => setText("") })
  }
  return (
    <form onSubmit={submit} className="space-y-2" noValidate>
      <Field label="Add a constraint" hint="Every option must respect it. The next round of the debate sees it." error={error}>
        {(p) => <Textarea {...p} rows={2} maxLength={2000} value={text} onChange={(e) => setText(e.target.value)} />}
      </Field>
      {add.isError ? (
        <p role="alert" className="text-sm text-destructive">
          {add.error.message}
        </p>
      ) : null}
      <Button type="submit" variant="outline" disabled={add.isPending}>
        {add.isPending ? "Adding…" : "Add constraint"}
      </Button>
    </form>
  )
}

/** The members of a council, known from what they contributed. */
function Members({ contributions }: { contributions: Contribution[] }) {
  const members = new Map(contributions.map((k) => [k.member, k]))
  if (members.size === 0) return <p className="text-sm text-muted-foreground">Members show up here as they contribute.</p>
  return (
    <ul className="flex flex-wrap gap-2" aria-label="Members">
      {[...members.values()].map((k) => (
        <li key={k.member}>
          <StatusBadge>
            <span className="font-mono">{k.member}</span>
            {k.role === DEVILS_ADVOCATE ? <span className="ml-1">devil&apos;s advocate</span> : null}
          </StatusBadge>
        </li>
      ))}
    </ul>
  )
}

function Stages({ contributions }: { contributions: Contribution[] }) {
  return (
    <>
      {COUNCIL_STAGES.filter((s) => s !== "synthesis").map((stage) => {
        const ofStage = contributions.filter((k) => k.stage === stage)
        if (ofStage.length === 0) return null
        const rounds = [...new Set(ofStage.map((k) => k.round))].sort((a, b) => a - b)
        return (
          <section key={stage} aria-label={`${stage} stage`} className="space-y-2">
            <h2 className="text-lg font-semibold capitalize">{stage === "critique" ? "Critiques" : stage === "revision" ? "Revisions" : "Proposals"}</h2>
            {rounds.map((round) => (
              <div key={round} className="space-y-2">
                {rounds.length > 1 ? <h3 className="text-sm font-medium text-muted-foreground">Round {round}</h3> : null}
                <ul className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
                  {ofStage
                    .filter((k) => k.round === round)
                    .map((k) => (
                      <li key={k.id}>
                        <ContributionCard contribution={k} />
                      </li>
                    ))}
                </ul>
              </div>
            ))}
          </section>
        )
      })}
    </>
  )
}

function CouncilView({ council: c }: { council: CouncilDetail }) {
  const projects = useProjects()
  const project = projects.data?.find((p) => p.id === c.project_id)?.name
  const constraints = c.constraints ?? []
  return (
    <>
      <PageHeader
        title={c.ref}
        badge={<StatusBadge tone={councilTone(c.status)}>{c.status}</StatusBadge>}
        meta={
          <>
            {project ? <span className="font-mono">{project}</span> : null} {project ? "· " : ""}
            {words(c.kind)} council, started <Age iso={c.created_at} ago />
          </>
        }
      >
        {c.gate?.status === "pending" ? (
          <Link to="/inbox" search={{ gate: c.gate.ref }} className={cn(buttonVariants())}>
            Decide in the Inbox
          </Link>
        ) : null}
      </PageHeader>
      <div className="space-y-6">
        {c.status === "failed" ? (
          <Alert variant="destructive">
            <AlertTitle>This council failed</AlertTitle>
            <AlertDescription className="text-destructive!">{c.gate?.status === "rejected" ? "Its brief was rejected." : "It could not finish."}</AlertDescription>
          </Alert>
        ) : null}
        {c.status === "canceled" ? (
          <Alert>
            <AlertTitle>This council was canceled</AlertTitle>
          </Alert>
        ) : null}
        <section aria-label="Question" className="space-y-2">
          <h2 className="text-lg font-semibold">Question</h2>
          <p className="whitespace-pre-wrap">{c.question}</p>
          <div>
            <h3 className="text-sm font-medium text-muted-foreground">Constraints</h3>
            {constraints.length ? (
              <ul className="list-disc pl-5 text-sm">
                {constraints.map((t, i) => (
                  <li key={i}>{t}</li>
                ))}
              </ul>
            ) : (
              <p className="text-sm text-muted-foreground">None.</p>
            )}
          </div>
          {c.status === "running" ? <ConstraintForm council={c} /> : null}
        </section>
        <section aria-label="Members" className="space-y-2">
          <h2 className="text-lg font-semibold">Members</h2>
          <Members contributions={c.contributions ?? []} />
        </section>
        <p className="text-xs text-muted-foreground">What the members wrote is agent text. It is shown as plain text and nothing in it is followed as an instruction.</p>
        <Stages contributions={c.contributions ?? []} />
        <section aria-label="Brief" className="space-y-2">
          <h2 className="text-lg font-semibold">Brief</h2>
          {c.brief ? (
            <BriefView brief={c.brief} />
          ) : (
            <p className="text-sm text-muted-foreground">{c.status === "running" ? "The brief is written after the revisions." : "No brief was written."}</p>
          )}
        </section>
        {c.decision ? (
          <section aria-label="Decision" className="space-y-1">
            <h2 className="text-lg font-semibold">Decision</h2>
            <p>
              <span className="font-mono">{decisionRef(c.decision.number)}</span>: <strong className="font-mono">{c.decision.choice}</strong>, decided by {c.decision.decided_by}.
            </p>
            {c.gate ? (
              <p className="text-sm text-muted-foreground">
                Gate <span className="font-mono">{c.gate.ref}</span> was {words(c.gate.status)}.
              </p>
            ) : null}
          </section>
        ) : null}
      </div>
    </>
  )
}

export function CouncilPage() {
  const { ref } = useParams({ from: "/app/councils/$ref" })
  const council = useCouncil(ref)
  return <Async query={council}>{(c) => <CouncilView council={c} />}</Async>
}

export function NewCouncilPage() {
  const navigate = useNavigate()
  const search = useSearch({ from: "/app/councils/new" })
  const start = useStartCouncil()
  const [project, setProject] = useState(search.project ?? "")
  const [question, setQuestion] = useState(search.question ?? "")
  const [kind, setKind] = useState("")
  const [constraints, setConstraints] = useState("")
  const [errors, setErrors] = useState<{ project?: string; question?: string }>({})

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    const next: typeof errors = {}
    if (!project) next.project = "Choose a project."
    if (!question.trim()) next.question = "Ask the council a question."
    setErrors(next)
    if (next.project || next.question) return
    start.mutate(
      { project, question: question.trim(), kind: kind || undefined, constraints: splitLines(constraints) },
      { onSuccess: (c) => void navigate({ to: "/councils/$ref", params: { ref: c.ref } }) },
    )
  }

  return (
    <>
      <PageHeader title="Start a council" />
      <form onSubmit={submit} className="max-w-xl space-y-4" noValidate>
        <Field label="Project" error={errors.project}>
          {(p) => <ProjectSelect {...p} className="w-full" value={project} onChange={setProject} />}
        </Field>
        <Field label="Question" hint="One question the council answers with a decision brief." error={errors.question}>
          {(p) => <Textarea {...p} rows={3} value={question} onChange={(e) => setQuestion(e.target.value)} />}
        </Field>
        <Field label="Kind" hint="Picks the members. Ad hoc is a general council.">
          {(p) => (
            <NativeSelect {...p} className="w-full" value={kind} onChange={(e) => setKind(e.target.value)}>
              <NativeSelectOption value="">Ad hoc</NativeSelectOption>
              {COUNCIL_KINDS.filter((k) => k !== "adhoc").map((k) => (
                <NativeSelectOption key={k} value={k}>
                  {words(k)}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          )}
        </Field>
        <Field label="Constraints" hint="Optional, one per line. Every option must respect them.">
          {(p) => <Textarea {...p} rows={3} value={constraints} onChange={(e) => setConstraints(e.target.value)} />}
        </Field>
        {start.isError ? (
          <p role="alert" className="text-sm text-destructive">
            {start.error.message}
          </p>
        ) : null}
        <Button type="submit" disabled={start.isPending}>
          {start.isPending ? "Starting…" : "Start council"}
        </Button>
      </form>
    </>
  )
}
