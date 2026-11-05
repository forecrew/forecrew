import { useEffect, useRef, useState } from "react"
import { useDecide } from "@/api/queries"
import type { GateInfo } from "@/api/types"
import { Age } from "@/components/Age"
import { ExternalLink } from "@/components/ExternalLink"
import { Field } from "@/components/Field"
import { Markdown } from "@/components/Markdown"
import { StatusBadge } from "@/components/StatusBadge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { Textarea } from "@/components/ui/textarea"
import type { GateKind } from "@/api/types"
import { cn } from "cn"
import { gateKindLabel } from "@/lib/format"

type Mode = "changes" | "reject" | null

function isTyping(el: EventTarget | null): boolean {
  return el instanceof HTMLElement && (el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName))
}

const KIND_ORDER: GateKind[] = ["merge_approval", "spec_approval", "profile_approval", "test_plan_approval", "needs_you"]
const KIND_CHIP: Record<GateKind, string> = {
  merge_approval: "Merges",
  spec_approval: "Specs",
  profile_approval: "Profiles",
  test_plan_approval: "Test plans",
  needs_you: "Needs you",
  assumption_review: "Assumptions",
  council_decision: "Councils",
  knowledge_proposal: "Knowledge",
  access_request: "Access",
  improvement_proposal: "Improvements",
}

type Decision = "approve" | "request-changes" | "reject"

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

function GateCard({ gate, selected, mode, setMode, onSelect, note, setNote, onDecide, pending, failure, single }: CardProps) {
  const [error, setError] = useState("")
  const ref = useRef<HTMLElement>(null)
  const feedbackRef = useRef<HTMLTextAreaElement>(null)
  const needsYou = gate.kind === "needs_you"

  useEffect(() => {
    if (selected) ref.current?.scrollIntoView?.({ block: "nearest" })
  }, [selected])
  useEffect(() => {
    if (mode) feedbackRef.current?.focus()
  }, [mode])

  const send = () => {
    if (mode === "changes" && !note.trim()) {
      setError("Feedback is required to request changes.")
      feedbackRef.current?.focus()
      return
    }
    setError("")
    onDecide(mode === "changes" ? "request-changes" : "reject", note)
  }

  const title = `${gate.ref} · ${gate.title}`
  return (
    <article ref={ref} aria-labelledby={`${gate.ref}-title`} aria-current={selected ? "true" : undefined} onFocus={onSelect} onClick={onSelect}>
      <Card className={selected && !single ? "ring-2 ring-ring" : ""}>
        <CardHeader className="flex flex-row flex-wrap items-center gap-2">
          <StatusBadge tone={needsYou ? "attention" : "neutral"}>{gateKindLabel(gate.kind)}</StatusBadge>
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
            {mode === null ? (
              <>
                <Button variant={selected ? "default" : "outline"} onClick={() => onDecide("approve", note)} disabled={pending}>
                  {needsYou ? "Approve and retry" : "Approve"}
                </Button>
                {needsYou ? null : (
                  <Button variant="outline" onClick={() => setMode("changes")}>
                    Request changes
                  </Button>
                )}
                <Button variant="outline" onClick={() => setMode("reject")}>
                  Reject
                </Button>
              </>
            ) : (
              <>
                <Button variant={mode === "reject" ? "destructive" : "default"} onClick={send} disabled={pending}>
                  {mode === "changes" ? "Send changes request" : "Confirm reject"}
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
          <Markdown>{gate.summary}</Markdown>
          {mode === null ? (
            <Field key="note" label="Note (optional)">
              {(p) => <Textarea {...p} rows={2} value={note} onChange={(e) => setNote(e.target.value)} />}
            </Field>
          ) : (
            <Field
              key="feedback"
              label={mode === "changes" ? "What should change?" : "Why stop? (optional)"}
              error={error}
            >
              {(p) => (
                <Textarea {...p} ref={feedbackRef} rows={3} value={note} onChange={(e) => setNote(e.target.value)} required={mode === "changes"} />
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
  const [kind, setKind] = useState<GateKind | "">("")
  const kinds = KIND_ORDER.filter((k) => all.some((g) => g.kind === k))
  const active = kind && kinds.includes(kind) ? kind : ""
  const gates = active ? all.filter((g) => g.kind === active) : all
  const [index, setIndex] = useState(() => Math.max(0, all.findIndex((g) => g.ref === initialRef)))
  const [mode, setMode] = useState<Mode>(null)
  const [notes, setNotes] = useState<Record<string, string>>({})
  const count = gates.length
  const current = Math.min(index, Math.max(count - 1, 0))
  const selected = gates[current]

  const onDecide = (ref: string, d: Decision, feedback: string) => {
    decide.mutate(
      { ref, decision: d, feedback: feedback.trim() || undefined },
      {
        onSuccess: () => {
          setMode(null)
          setNotes((prev) => {
            const next = { ...prev }
            delete next[ref]
            return next
          })
        },
      },
    )
  }

  useEffect(() => {
    if (!keyboard) return
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || isTyping(e.target) || document.querySelector('[role="dialog"]')) return
      switch (e.key.toLowerCase()) {
        case "j":
          setIndex(Math.min(current + 1, Math.max(count - 1, 0)))
          setMode(null)
          break
        case "k":
          setIndex(Math.max(current - 1, 0))
          setMode(null)
          break
        case "a":
          if (selected && !decide.isPending) onDecide(selected.ref, "approve", notes[selected.ref] ?? "")
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
      onDecide={(d, f) => onDecide(g.ref, d, f)}
      pending={decide.isPending && decide.variables?.ref === g.ref}
      failure={decide.isError && decide.variables?.ref === g.ref ? decide.error.message : undefined}
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
              {k === "" ? `All ${all.length}` : KIND_CHIP[k]}
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
                  <span className="font-medium">{gateKindLabel(g.kind)}</span>
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
