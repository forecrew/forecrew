import { Link, useNavigate, useParams } from "@tanstack/react-router"
import { useState } from "react"
import { ApiError, api } from "@/api/client"
import { useGoals, useProject } from "@/api/queries"
import type { Profile } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { ExternalLink } from "@/components/ExternalLink"
import { Field } from "@/components/Field"
import { Markdown } from "@/components/Markdown"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"
import { goalRef, goalTone, isRefOrId, jobRef, projectTone, splitList, stageOf, words } from "@/lib/format"

function ProfileView({ profile }: { profile: Profile }) {
  return (
    <div className="space-y-4 rounded-xl border bg-card p-4">
      <Markdown>{profile.summary}</Markdown>
      <dl className="grid gap-2 text-sm sm:grid-cols-[10rem_1fr]">
        {profile.languages?.length ? (
          <>
            <dt className="text-muted-foreground">Languages</dt>
            <dd>{profile.languages.join(", ")}</dd>
          </>
        ) : null}
        {profile.platforms?.length ? (
          <>
            <dt className="text-muted-foreground">Platforms</dt>
            <dd>{profile.platforms.join(", ")}</dd>
          </>
        ) : null}
      </dl>
      {profile.components?.length ? (
        <TableWrap label="Components">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Component</TableHead>
                <TableHead>Build</TableHead>
                <TableHead>Test</TableHead>
                <TableHead>Platforms</TableHead>
                <TableHead>Needs</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {profile.components.map((c) => (
                <TableRow key={c.path}>
                  <TableCell>
                    <span className="font-mono">{c.path}</span> <span className="text-muted-foreground">{c.stack}</span>
                  </TableCell>
                  <TableCell className="font-mono">{c.build || "unknown"}</TableCell>
                  <TableCell className="font-mono">{c.test || "unknown"}</TableCell>
                  <TableCell>{c.platforms?.join(", ")}</TableCell>
                  <TableCell>
                    {c.needs?.map((n) => (
                      <StatusBadge key={n} className="mr-1">
                        {n}
                      </StatusBadge>
                    ))}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </TableWrap>
      ) : null}
    </div>
  )
}

interface DispatchErrors {
  role?: string
  prompt?: string
  goal?: string
  task?: string
  form?: string
}

function DispatchForm({ project }: { project: string }) {
  const navigate = useNavigate()
  const [role, setRole] = useState("reviewer")
  const [prompt, setPrompt] = useState("")
  const [needs, setNeeds] = useState("")
  const [goal, setGoal] = useState("")
  const [task, setTask] = useState("")
  const [baseRef, setBaseRef] = useState("")
  const [branch, setBranch] = useState("")
  const [feedback, setFeedback] = useState("")
  const [facts, setFacts] = useState(false)
  const [advanced, setAdvanced] = useState(false)
  const [errors, setErrors] = useState<DispatchErrors>({})
  const [busy, setBusy] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const next: DispatchErrors = {}
    if (!role.trim()) next.role = "Enter a role."
    if (!prompt.trim()) next.prompt = "Tell the job what to do."
    if (goal.trim() && !isRefOrId("GOAL", goal.trim())) next.goal = "Enter a goal ref like GOAL-3, or its ID."
    if (task.trim() && !isRefOrId("TASK", task.trim())) next.task = "Enter a task ref like TASK-12, or its ID."
    setErrors(next)
    if (Object.keys(next).length) {
      if (next.goal || next.task) setAdvanced(true)
      return
    }
    setBusy(true)
    try {
      const job = await api.dispatchJob({
        project,
        role: role.trim(),
        prompt,
        needs: splitList(needs),
        goal: goal.trim() || undefined,
        task: task.trim() || undefined,
        base_ref: baseRef.trim() || undefined,
        branch: branch.trim() || undefined,
        feedback: feedback.trim() || undefined,
        include_repo_facts: facts || undefined,
      })
      await navigate({ to: "/jobs/$ref", params: { ref: jobRef(job.number) } })
    } catch (err) {
      setErrors({ form: err instanceof ApiError ? err.message : "Could not start the job." })
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={(e) => void submit(e)} className="max-w-xl space-y-4" noValidate>
      <Field label="Role" hint="A read-only role, for example reviewer." error={errors.role}>
        {(p) => <Input {...p} className="font-mono" value={role} onChange={(e) => setRole(e.target.value)} />}
      </Field>
      <Field label="Prompt" error={errors.prompt}>
        {(p) => <Textarea {...p} rows={4} value={prompt} onChange={(e) => setPrompt(e.target.value)} />}
      </Field>
      <Field label="Needs" hint="Optional worker labels, separated by commas: linux, docker.">
        {(p) => <Input {...p} className="font-mono" value={needs} onChange={(e) => setNeeds(e.target.value)} />}
      </Field>
      <details open={advanced} onToggle={(e) => setAdvanced(e.currentTarget.open)} className="rounded-xl border bg-card p-3">
        <summary className="cursor-pointer py-2 text-sm font-medium">Advanced</summary>
        <div className="mt-2 space-y-4">
          <Field label="Goal" hint="Optional: ties the job to a goal, for example GOAL-3." error={errors.goal}>
            {(p) => <Input {...p} className="font-mono" autoComplete="off" value={goal} onChange={(e) => setGoal(e.target.value)} />}
          </Field>
          <Field label="Task" hint="Optional: ties the job to a task, for example TASK-12." error={errors.task}>
            {(p) => <Input {...p} className="font-mono" autoComplete="off" value={task} onChange={(e) => setTask(e.target.value)} />}
          </Field>
          <Field label="Base ref" hint="Optional branch or commit to start from. Defaults to the project's default branch.">
            {(p) => <Input {...p} className="font-mono" autoComplete="off" spellCheck={false} value={baseRef} onChange={(e) => setBaseRef(e.target.value)} />}
          </Field>
          <Field label="Branch" hint="Optional branch to check out and inspect. A read-only job never pushes it.">
            {(p) => <Input {...p} className="font-mono" autoComplete="off" spellCheck={false} value={branch} onChange={(e) => setBranch(e.target.value)} />}
          </Field>
          <Field label="Feedback" hint="Optional notes for the job, for example what to look at.">
            {(p) => <Textarea {...p} rows={3} value={feedback} onChange={(e) => setFeedback(e.target.value)} />}
          </Field>
          <div className="flex items-center gap-2">
            <input id="dispatch-facts" type="checkbox" className="size-4" checked={facts} onChange={(e) => setFacts(e.target.checked)} />
            <Label htmlFor="dispatch-facts">Include repository facts</Label>
          </div>
        </div>
      </details>
      {errors.form ? (
        <p role="alert" className="text-sm text-destructive">
          {errors.form}
        </p>
      ) : null}
      <Button type="submit" disabled={busy}>
        {busy ? "Starting…" : "Run job"}
      </Button>
    </form>
  )
}

export function ProjectPage() {
  const { name } = useParams({ from: "/app/projects/$name" })
  const project = useProject(name)
  const goals = useGoals(name)
  return (
    <Async query={project}>
      {(p) => (
        <>
          <PageHeader title={p.name} />
          <div className="mb-6 flex flex-wrap items-center gap-3 text-sm">
            <StatusBadge tone={projectTone(p.status)}>{p.status}</StatusBadge>
            <ExternalLink href={`https://github.com/${p.repo}`}>{p.repo}</ExternalLink>
            <span>
              Default branch <span className="font-mono">{p.default_branch}</span>
            </span>
          </div>

          <section aria-labelledby="profile" className="mb-8">
            <h2 id="profile" className="mb-2 text-lg font-semibold">
              Profile
            </h2>
            {p.profile ? (
              <ProfileView profile={p.profile} />
            ) : (
              <p className="text-muted-foreground">
                {p.status === "onboarding" ? "Onboarding is running. The profile shows up once you approve it in the inbox." : "No approved profile yet."}
              </p>
            )}
          </section>

          <section aria-labelledby="project-goals" className="mb-8">
            <h2 id="project-goals" className="mb-2 text-lg font-semibold">
              Goals
            </h2>
            <Async query={goals} isEmpty={(d) => d.length === 0} empty="No goals for this project.">
              {(items) => (
                <ul className="divide-y rounded-xl border bg-card">
                  {items.map((g) => (
                    <li key={g.id} className="flex flex-wrap items-center gap-3 p-3 text-sm">
                      <Link to="/goals/$ref" params={{ ref: goalRef(g.number) }} className="inline-flex flex-1 items-center gap-2 underline-offset-2 hover:underline">
                        <span className="font-mono">{goalRef(g.number)}</span> {g.title}
                      </Link>
                      <StatusBadge tone={goalTone(g.status)}>{words(g.status)}</StatusBadge>
                      <span className="text-muted-foreground">{stageOf(g.status)}</span>
                      <Age iso={g.updated_at} />
                    </li>
                  ))}
                </ul>
              )}
            </Async>
          </section>

          <section aria-labelledby="adhoc">
            <h2 id="adhoc" className="mb-2 text-lg font-semibold">
              Run ad-hoc job
            </h2>
            <DispatchForm project={p.name} />
          </section>
        </>
      )}
    </Async>
  )
}
