import { useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useRouterState } from "@tanstack/react-router"
import { AlertCircleIcon, LockIcon, XIcon } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import { api, ApiError } from "@/api/client"
import { keys } from "@/api/queries"
import type { AssistantView, ConfigPlan } from "@/api/types"
import { Markdown } from "@/components/Markdown"
import { PlanCard } from "@/components/PlanCard"
import { StatusBadge } from "@/components/StatusBadge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"
import { looksLikeGoal, looksLikeSecret } from "@/lib/secrets"

/** A page of the app by the name people know it by. */
const PAGES: [prefix: string, label: string][] = [
  ["/machines/add", "Add machines"],
  ["/machines", "Machines"],
  ["/settings/history", "Change history"],
  ["/settings/policies", "Policies"],
  ["/settings/people", "People"],
  ["/settings/sign-in", "Sign-in"],
  ["/settings", "Settings"],
  ["/jobs", "Jobs"],
  ["/inbox", "Inbox"],
  ["/projects", "Projects"],
]
const pageLabel = (path: string) => PAGES.find(([p]) => path === p || path.startsWith(p + "/"))?.[1]

/** Starters matched to the page; the same four everywhere would not help anyone. */
function starters(path: string): string[] {
  const here = pageLabel(path)
  const base = ["Add a machine", "Invite my team", "Why is my job waiting?", "What can you do?"]
  if (here === "Machines" || here === "Add machines") return ["Add my Linux server", "Add a Windows PC", "Why is my job waiting?", "Which machines can run Windows?"]
  if (here === "People") return ["Invite my team", "Who can merge in Mobile?", "Add a team", "What can you do?"]
  return base
}

type Entry = { at: string; key: string } & ({ kind: "message"; author: "user" | "assistant"; body: string } | { kind: "plan"; plan: ConfigPlan })

/** The messages and the plans of a conversation, in the order they happened. */
function entries(view: AssistantView | undefined): Entry[] {
  if (!view) return []
  const all: Entry[] = [
    ...view.messages.map((m) => ({ at: m.created_at, key: m.id, kind: "message" as const, author: m.author, body: m.body })),
    ...view.plans.map((p) => ({ at: p.created_at, key: p.id, kind: "plan" as const, plan: p })),
  ]
  return all.sort((a, b) => Date.parse(a.at) - Date.parse(b.at))
}

/** Why the assistant cannot answer, in words, with what to do. */
function unavailableText(err: ApiError): string {
  return err.status === 503 ? `The assistant is not running: ${err.message.replace(/: unavailable$/, "")}. Every setting still works with forms.` : err.message
}

interface Props {
  conversation?: string
  onConversation: (id: string | undefined) => void
  /** What to send as soon as the panel opens, from the command menu or an Ask button. */
  initial?: string
  onClose: () => void
}

/**
 * The assistant: say what you want, read the plan, confirm with its button. Text is plain
 * markdown; everything that can do something is a card drawn from server data.
 */
export function AssistantPanel({ conversation, onConversation, initial, onClose }: Props) {
  const qc = useQueryClient()
  const path = useRouterState({ select: (s) => s.location.pathname })
  const [text, setText] = useState(initial ?? "")
  const [withPage, setWithPage] = useState(true)
  const [error, setError] = useState("")
  const [down, setDown] = useState("")
  const [sending, setSending] = useState(false)
  const [stopped, setStopped] = useState(false)
  const view = useQuery({
    queryKey: keys.conversation(conversation ?? ""),
    queryFn: () => api.getConversation(conversation!),
    enabled: Boolean(conversation),
    // Each turn is a job: poll until it answers.
    refetchInterval: (q) => (q.state.data?.thinking && !stopped ? 2000 : false),
  })
  const items = entries(view.data)
  const thinking = (view.data?.thinking ?? false) && !stopped
  const box = useRef<HTMLTextAreaElement>(null)
  const secret = looksLikeSecret(text)

  // The panel never steals focus when a plan arrives, but it takes it when it opens.
  useEffect(() => box.current?.focus(), [])

  const send = async (message: string) => {
    const body = message.trim()
    if (!body || sending) return
    if (looksLikeSecret(body)) return // the notice under the box says why
    setSending(true)
    setError("")
    setDown("")
    setStopped(false)
    try {
      const v = await api.sendAssistant({ message: body, conversation, page: withPage ? path : undefined })
      qc.setQueryData(keys.conversation(v.id), v)
      onConversation(v.id)
      setText("")
    } catch (err) {
      if (err instanceof ApiError && err.status === 503) setDown(unavailableText(err))
      else setError(err instanceof Error ? err.message : "Could not send it")
    } finally {
      setSending(false)
    }
  }

  const stop = async () => {
    setStopped(true)
    const turn = [...(view.data?.messages ?? [])].reverse().find((m) => m.author === "user")
    const job = (turn as { job_id?: string } | undefined)?.job_id
    if (job) await api.cancelJob(job).catch(() => {})
  }

  return (
    <section aria-label="Assistant" className="flex h-full min-h-0 flex-col">
      <header className="flex items-center gap-2 border-b px-4 py-3">
        <h2 className="flex-1 text-base font-medium">Assistant</h2>
        <Button variant="ghost" size="sm" onClick={() => onConversation(undefined)}>
          New
        </Button>
        <Button variant="ghost" size="icon" aria-label="Close the assistant" onClick={onClose}>
          <XIcon aria-hidden="true" />
        </Button>
      </header>
      <div className="min-h-0 flex-1 space-y-3 overflow-y-auto px-4 py-3">
        {down ? (
          <Alert>
            <AlertCircleIcon aria-hidden="true" />
            <AlertTitle>The assistant is not available</AlertTitle>
            <AlertDescription>{down}</AlertDescription>
          </Alert>
        ) : null}
        {items.length === 0 && !thinking ? (
          <div className="space-y-3">
            <p className="text-sm text-muted-foreground">Tell me what you have or what you want. Nothing changes until you confirm.</p>
            <ul aria-label="Starter prompts" className="flex flex-wrap gap-2">
              {starters(path).map((s) => (
                <li key={s}>
                  <Button variant="outline" size="sm" onClick={() => void send(s)}>
                    {s}
                  </Button>
                </li>
              ))}
            </ul>
          </div>
        ) : null}
        <div role="log" aria-live="polite" aria-label="Conversation" className="space-y-3">
          {items.map((e) =>
            e.kind === "plan" ? (
              <PlanCard key={e.key} plan={e.plan} onChange={() => void qc.invalidateQueries({ queryKey: keys.conversation(conversation ?? "") })} />
            ) : (
              <article key={e.key} aria-label={e.author === "user" ? "You" : "Assistant"} className="space-y-1">
                <p className="text-xs font-medium text-muted-foreground">{e.author === "user" ? "You" : "Assistant"}</p>
                {e.author === "user" ? <p className="text-sm break-words whitespace-pre-wrap">{e.body}</p> : <Markdown>{e.body}</Markdown>}
              </article>
            ),
          )}
          {thinking ? (
            <p role="status" className="flex items-center gap-2 text-sm text-muted-foreground">
              Thinking…
              <Button variant="outline" size="sm" onClick={() => void stop()}>
                Stop
              </Button>
            </p>
          ) : null}
        </div>
      </div>
      <form
        className="space-y-2 border-t px-4 py-3"
        onSubmit={(e) => {
          e.preventDefault()
          void send(text)
        }}
      >
        {withPage && pageLabel(path) ? (
          <p className="flex items-center gap-1">
            <StatusBadge>Page: {pageLabel(path)}</StatusBadge>
            <Button type="button" variant="ghost" size="icon-xs" aria-label="Remove the page as context" onClick={() => setWithPage(false)}>
              <XIcon aria-hidden="true" />
            </Button>
          </p>
        ) : null}
        <label htmlFor="assistant-message" className="sr-only">
          Say what you want
        </label>
        <Textarea
          id="assistant-message"
          ref={box}
          rows={2}
          value={text}
          placeholder="Say what you want…"
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && !e.shiftKey) {
              e.preventDefault()
              void send(text)
            }
          }}
        />
        {secret ? (
          <p role="alert" className="text-sm text-destructive">
            That looks like a key, so it is not sent. Secrets go in a secure field, which the assistant draws in its plan.
          </p>
        ) : null}
        {error ? (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        ) : null}
        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" disabled={sending || !text.trim() || secret}>
            Send
          </Button>
          {looksLikeGoal(text) && !secret ? (
            <Button variant="outline" render={<Link to="/goals/new" search={{ title: text.trim() } as never} onClick={onClose} />}>
              Start this as a goal
            </Button>
          ) : null}
        </div>
        <p className="flex items-center gap-1 text-xs text-muted-foreground">
          <LockIcon className="size-3" aria-hidden="true" /> Sees your configuration. Never your secrets.
        </p>
      </form>
    </section>
  )
}
