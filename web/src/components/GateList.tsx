import { Link } from "@tanstack/react-router"
import { useEffect, useRef, useState } from "react"
import { useAssumptions, useCouncil, useDecide, useVerifyAssumption } from "@/api/queries"
import type { GateInfo } from "@/api/types"
import { Age } from "@/components/Age"
import { AssumptionDetails } from "@/components/AssumptionReview"
import { BriefView } from "@/components/CouncilBrief"
import { ExternalLink } from "@/components/ExternalLink"
import { Field } from "@/components/Field"
import { Markdown } from "@/components/Markdown"
import { StatusBadge } from "@/components/StatusBadge"
import { VerificationSection } from "@/components/Verification"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { Textarea } from "@/components/ui/textarea"
import { cn } from "cn"
import { assumptionOfGate, councilOfGate, gateLabel, isQuestion } from "@/lib/format"

type Mode = "changes" | "reject" | "verify" | null

function isTyping(el: EventTarget | null): boolean {
  return el instanceof HTMLElement && (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName))
}

// The filter chips. A question a job asked (ask_human) has a chip of its own, apart from the escalations.
type Chip = GateInfo["kind"] | "question"
const CHIP_ORDER: Chip[] = [
  "merge_approval", "spec_approval", "council_decision", "assumption_review", "profile_approval", "test_plan_approval",
  "knowledge_proposal", "access_request", "improvement_proposal", "needs_you", "question",
]
const CHIP_LABEL: Record<Chip, string> = {
  merge_approval: "Merges",
  spec_approval: "Specs",
  council_decision: "Councils",
  assumption_review: "Assumptions",
  profile_approval: "Profiles",
  test_plan_approval: "Test plans",
  knowledge_proposal: "Knowledge",
  access_request: "Access",
  improvement_proposal: "Improvements",
  needs_you: "Needs you",
  question: "Questions",
}
const chipOf = (g: GateInfo): Chip => (isQuestion(g) ? "question" : g.kind)

/** "verify" resolves the assumption of an assumption_review gate with evidence, which closes the gate. */
type Decision = "approve" | "request-changes" | "reject" | "verify"

/** The words of a gate's actions, by what it asks. */
function wordsFor(g: GateInfo, humanRoundUsed: boolean) {
  const question = isQuestion(g)
  const council = g.kind === "council_decision"
  const assumption = g.kind === "assumption_review"
  return {
    approve: question ? "Send answer" : g.kind === "needs_you" ? "Approve and retry" : council ? "Approve recommendation" : assumption ? "Accept" : "Approve",
    reject: question ? "Decline" : assumption ? "Invalidate" : "Reject",
    confirmReject: question ? "Confirm decline" : assumption ? "Confirm invalidate" : "Confirm reject",
    rejectField: assumption ? "Why is it wrong? (optional)" : question ? "Why decline? (optional)" : "Why stop? (optional)",
    changesField: council ? "Constraint for one more round" : assumption ? "What should be checked?" : "What should change?",
    sendChanges: council ? "Send constraint" : "Send changes request",
    note: question ? "Your answer" : "Note (optional)",
    canChange: g.kind !== "needs_you" && !(council && humanRoundUsed),
  }
}

interface CardProps {
  gate: GateInfo
  selected: boolean
  mode: Mode
  setMode: (m: Mode) => void
  onSelect: () => void
  note: string
  setNote: (note: string) => void
  onDecide: (decision: Decision, feedback: string) => void
  pending: boolean
  failure?: string
  /** Shown alone in the inbox: no selection ring. */
  single?: boolean
}

/** What a card shows of a gate: a council's brief, an assumption, or the gate's own summary. */
function GateBody({ gate, assumption, council }: { gate: GateInfo; assumption?: React.ComponentProps<typeof AssumptionDetails>["assumption"]; council?: ReturnType<typeof useCouncil>["data"] }) {
  if (gate.kind === "council_decision" && council?.brief) {
    return (
      <div className="space-y-3">
        <p className="text-sm text-muted-foreground">
          Approve accepts the recommendation. Request changes adds your feedback as a constraint for one more round. Reject fails the council. What the council wrote is agent text.
        </p>
        <p className="text-sm">
          <Link to="/councils/$ref" params={{ ref: council.ref }} className="text-primary underline underline-offset-2">
            Open the whole debate ({council.ref})
          </Link>
        </p>
        <BriefView brief={council.brief} />
      </div>
    )
  }
  if (gate.kind === "assumption_review" && assumption) {
    return (
      <div className="space-y-3">
        <AssumptionDetails assumption={assumption} />
        <details>
          <summary className="cursor-pointer text-sm text-muted-foreground">Review notes</summary>
          <Markdown>{gate.summary}</Markdown>
        </details>
      </div>
    )
  }
  return <Markdown>{gate.summary}</Markdown>
}

function GateCard({ gate, selected, mode, setMode, onSelect, note, setNote, onDecide, pending, failure, single }: CardProps) {
  const [error, setError] = useState("")
  const ref = useRef<HTMLElement>(null)
  const feedbackRef = useRef<HTMLTextAreaElement>(null)
  const needsYou = gate.kind === "needs_you"
  const councilRef = gate.kind === "council_decision" ? councilOfGate(gate.title) : undefined
  const council = useCouncil(councilRef ?? "", Boolean(councilRef)).data
  const assumptionRef = gate.kind === "assumption_review" ? assumptionOfGate(gate.title) : undefined
  const assumption = useAssumptions(gate.project, Boolean(assumptionRef)).data?.find((a) => a.ref === assumptionRef)
  // A council gets one human round: after it the gate can only be approved or rejected.
  const roundUsed = (council?.contributions ?? []).filter((k) => k.stage === "synthesis").length >= 2
  const w = wordsFor(gate, roundUsed)

  useEffect(() => {
    if (selected) ref.current?.scrollIntoView?.({ block: "nearest" })
  }, [selected])
  useEffect(() => {
    if (mode) feedbackRef.current?.focus()
  }, [mode])
  // R on a council that had its human round opens nothing and says why.
  const refused = mode === "changes" && !w.canChange
  const shown = refused ? null : mode

  const send = () => {
    if (shown === "changes" && !note.trim()) {
      setError("Feedback is required to request changes.")
      feedbackRef.current?.focus()
      return
    }
    if (shown === "verify" && !note.trim()) {
      setError("Evidence is required to verify.")
      feedbackRef.current?.focus()
      return
    }
    setError("")
    onDecide(shown === "changes" ? "request-changes" : shown === "verify" ? "verify" : "reject", note)
  }

  const title = `${gate.ref} · ${gate.title}`
  return (
    <article ref={ref} aria-labelledby={`${gate.ref}-title`} aria-current={selected ? "true" : undefined} onFocus={onSelect} onClick={onSelect}>
      <Card className={selected && !single ? "ring-2 ring-ring" : ""}>
        <CardHeader className="flex flex-row flex-wrap items-center gap-2">
          <StatusBadge tone={needsYou && !isQuestion(gate) ? "attention" : "neutral"}>{gateLabel(gate)}</StatusBadge>
          <h2 id={`${gate.ref}-title`} className="min-w-0 flex-1 font-medium">
            {title}
          </h2>
          <span className="text-sm text-muted-foreground">
            waiting <Age iso={gate.created_at} />
          </span>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-sm text-muted-foreground">
            <span className="font-mono">{gate.project}</span>
            {gate.goal_ref ? <> · {gate.goal_ref}</> : null}
            {gate.task_ref ? <> · {gate.task_ref}</> : null}
          </p>
          <div className="flex flex-wrap items-center gap-2">
            {shown === null ? (
              <>
                <Button variant={selected ? "default" : "outline"} onClick={() => onDecide("approve", note)} disabled={pending}>
                  {w.approve}
                </Button>
                {gate.kind === "assumption_review" ? (
                  <Button variant="outline" onClick={() => setMode("verify")}>
                    Verify
                  </Button>
                ) : null}
                {w.canChange ? (
                  <Button variant="outline" onClick={() => setMode("changes")}>
                    Request changes
                  </Button>
                ) : null}
                <Button variant="outline" onClick={() => setMode("reject")}>
                  {w.reject}
                </Button>
              </>
            ) : (
              <>
                <Button variant={shown === "reject" ? "destructive" : "default"} onClick={send} disabled={pending}>
                  {shown === "changes" ? w.sendChanges : shown === "verify" ? "Confirm verified" : w.confirmReject}
                </Button>
                <Button
                  variant="ghost"
                  onClick={() => {
                    setMode(null)
                    setError("")
                  }}
                >
                  Cancel
                </Button>
              </>
            )}
            {gate.url ? (
              <ExternalLink href={gate.url} className="ml-auto text-sm">
                Open on GitHub
              </ExternalLink>
            ) : null}
          </div>
          <GateBody gate={gate} assumption={assumption} council={council} />
          {gate.kind === "merge_approval" && gate.url ? <VerificationSection change={gate.url} title={`${gate.ref} verification`} /> : null}
          {shown === null ? (
            <Field key="note" label={w.note} error={refused ? "This council had its human round: approve or reject its brief." : error}>
              {(p) => <Textarea {...p} rows={2} value={note} onChange={(e) => setNote(e.target.value)} />}
            </Field>
          ) : (
            <Field
              key="feedback"
              label={shown === "changes" ? w.changesField : shown === "verify" ? "Evidence that it holds" : w.rejectField}
              error={error}
            >
              {(p) => (
                <Textarea {...p} ref={feedbackRef} rows={3} value={note} onChange={(e) => setNote(e.target.value)} required={shown !== "reject"} />
              )}
            </Field>
          )}
          {failure ? (
            <p role="alert" className="text-sm text-destructive">
              {failure}
            </p>
          ) : null}
        </CardContent>
      </Card>
    </article>
  )
}

/**
 * Gates as decision cards. With keyboard on, J and K move, A approves and R
 * requests changes (the inbox); without it the cards are used by pointer and tab.
 */
export function GateList({ gates: all, keyboard = false, initialRef }: { gates: GateInfo[]; keyboard?: boolean; initialRef?: string }) {
  const decide = useDecide()
  const verify = useVerifyAssumption()
  const [kind, setKind] = useState<Chip | "">("")
  const [invalid, setInvalid] = useState<{ ref: string; message: string }>()
  const kinds = CHIP_ORDER.filter((k) => all.some((g) => chipOf(g) === k))
  const active = kind && kinds.includes(kind) ? kind : ""
  const gates = active ? all.filter((g) => chipOf(g) === active) : all
  const [index, setIndex] = useState(() => Math.max(0, all.findIndex((g) => g.ref === initialRef)))
  const [mode, setMode] = useState<Mode>(null)
  const [notes, setNotes] = useState<Record<string, string>>({})
  const count = gates.length
  const current = Math.min(index, Math.max(count - 1, 0))
  const selected = gates[current]

  const onDecide = (g: GateInfo, d: Decision, feedback: string) => {
    const ref = g.ref
    if (d === "approve" && isQuestion(g) && !feedback.trim()) {
      setInvalid({ ref, message: "Write the answer first: it is what the job gets." })
      return
    }
    setInvalid(undefined)
    const done = {
      onSuccess: () => {
        setMode(null)
        setNotes((prev) => {
          const next = { ...prev }
          delete next[ref]
          return next
        })
      },
    }
    if (d === "verify") {
      verify.mutate({ ref: assumptionOfGate(g.title) ?? "", evidence: feedback.trim() }, done)
      return
    }
    decide.mutate({ ref, decision: d, feedback: feedback.trim() || undefined }, done)
  }

  useEffect(() => {
    if (!keyboard) return
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || isTyping(e.target) || document.querySelector('[role="dialog"]')) return
      switch (e.key.toLowerCase()) {
        case "j":
          setIndex(Math.min(current + 1, Math.max(count - 1, 0)))
          setMode(null)
          setInvalid(undefined)
          break
        case "k":
          setIndex(Math.max(current - 1, 0))
          setMode(null)
          setInvalid(undefined)
          break
        case "a":
          if (selected && !decide.isPending) onDecide(selected, "approve", notes[selected.ref] ?? "")
          break
        case "r":
          if (selected && selected.kind !== "needs_you") {
            e.preventDefault()
            setMode("changes")
          }
          break
      }
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  })

  const cardFor = (g: GateInfo, i: number, single: boolean) => (
    <GateCard
      gate={g}
      selected={i === current}
      single={single}
      mode={i === current ? mode : null}
      setMode={setMode}
      onSelect={() => {
        if (i !== current) {
          setIndex(i)
          setMode(null)
        }
      }}
      note={notes[g.ref] ?? ""}
      setNote={(n) => setNotes((prev) => ({ ...prev, [g.ref]: n }))}
      onDecide={(d, f) => onDecide(g, d, f)}
      pending={(decide.isPending && decide.variables?.ref === g.ref) || (verify.isPending && verify.variables?.ref === assumptionOfGate(g.title))}
      failure={
        invalid?.ref === g.ref
          ? invalid.message
          : decide.isError && decide.variables?.ref === g.ref
            ? decide.error.message
            : verify.isError && verify.variables?.ref === assumptionOfGate(g.title)
              ? verify.error.message
              : undefined
      }
    />
  )

  if (!keyboard) {
    return (
      <ul className="space-y-4" aria-label="Pending decisions">
        {gates.map((g, i) => (
          <li key={g.ref}>{cardFor(g, i, false)}</li>
        ))}
      </ul>
    )
  }

  // The inbox: the list with its filters on the left, the selected decision on the right.
  return (
    <div className="grid grid-cols-1 items-start gap-6 lg:grid-cols-[20rem_1fr]">
      <div className="space-y-4">
        <div className="flex flex-wrap gap-2" role="group" aria-label="Filter decisions">
          {(["", ...kinds] as const).map((k) => (
            <Button key={k} size="sm" variant={k === active ? "default" : "outline"} aria-pressed={k === active} onClick={() => { setKind(k); setIndex(0); setMode(null) }}>
              {k === "" ? `All ${all.length}` : CHIP_LABEL[k]}
            </Button>
          ))}
        </div>
        <ul aria-label="Pending decisions" className="overflow-hidden rounded-xl border bg-card">
          {gates.map((g, i) => (
            <li key={g.ref} className="border-b last:border-b-0">
              <button
                type="button"
                aria-current={i === current ? "true" : undefined}
                onClick={() => {
                  setIndex(i)
                  setMode(null)
                }}
                className={cn("block w-full space-y-0.5 border-l-2 border-transparent px-3 py-2 text-left text-sm hover:bg-accent", i === current && "border-primary font-medium")}
              >
                <span className="flex justify-between gap-2 text-xs text-muted-foreground">
                  <span className="font-medium">{gateLabel(g)}</span>
                  <Age iso={g.created_at} ago />
                </span>
                <span className="block font-medium">{g.title}</span>
                <span className="block text-muted-foreground">
                  <span className="font-mono">{g.project}</span>
                  {g.goal_ref ? <> · {g.goal_ref}</> : null}
                </span>
              </button>
            </li>
          ))}
        </ul>
        <p className="text-sm text-muted-foreground">
          Keyboard: <kbd className="font-mono">J</kbd> and <kbd className="font-mono">K</kbd> move, <kbd className="font-mono">A</kbd> approves,{" "}
          <kbd className="font-mono">R</kbd> requests changes.
        </p>
      </div>
      {selected ? cardFor(selected, current, true) : null}
    </div>
  )
}
