import { useNavigate, useSearch, Link } from "@tanstack/react-router"
import { useState } from "react"
import { ApiError, api } from "@/api/client"
import { useGoals, useProjects } from "@/api/queries"
import { GOAL_STATUSES, type GoalStatus } from "@/api/types"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { Field } from "@/components/Field"
import { PageHeader } from "@/components/PageHeader"
import { ProjectSelect } from "@/components/ProjectSelect"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { cn } from "cn"
import { Button, buttonVariants } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Textarea } from "@/components/ui/textarea"
import { goalRef, goalTone, splitList, stageOf, words } from "@/lib/format"

export function GoalsPage() {
  const { project = "", status = "" } = useSearch({ from: "/app/goals" })
  const navigate = useNavigate({ from: "/goals" })
  const goals = useGoals(project, status as GoalStatus | "")
  const projects = useProjects()
  const names = new Map(projects.data?.map((p) => [p.id, p.name]))
  const setFilter = (next: { project?: string; status?: string }) =>
    void navigate({
      search: (prev) => ({ ...prev, ...Object.fromEntries(Object.entries(next).map(([k, v]) => [k, v || undefined])) }),
    })

  return (
    <>
      <PageHeader title="Goals">
        <Link to="/goals/new" className={cn(buttonVariants())}>
          New goal
        </Link>
      </PageHeader>
      <div className="mb-4 flex flex-wrap gap-4">
        <div className="space-y-1.5">
          <Label htmlFor="goal-project">Project</Label>
          <ProjectSelect id="goal-project" allLabel="All projects" value={project} onChange={(v) => setFilter({ project: v })} />
        </div>
        <div className="space-y-1.5">
          <Label htmlFor="goal-status">Status</Label>
          <NativeSelect id="goal-status" className="w-full sm:w-48" value={status} onChange={(e) => setFilter({ status: e.target.value })}>
            <NativeSelectOption value="">All statuses</NativeSelectOption>
            {GOAL_STATUSES.map((s) => (
              <NativeSelectOption key={s} value={s}>
                {words(s)}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
      </div>
      <Async query={goals} isEmpty={(d) => d.length === 0} empty="No goals yet. Create one with New goal.">
        {(items) => (
          <TableWrap label="Goals">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Goal</TableHead>
                  <TableHead>Project</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Stage</TableHead>
                  <TableHead>Updated</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((g) => (
                  <TableRow key={g.id}>
                    <TableCell>
                      <Link to="/goals/$ref" params={{ ref: goalRef(g.number) }} className="inline-flex items-center gap-2 underline-offset-2 hover:underline">
                        <span className="font-mono">{goalRef(g.number)}</span> {g.title}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">{names.get(g.project_id) ?? ""}</TableCell>
                    <TableCell>
                      <StatusBadge tone={goalTone(g.status)}>{words(g.status)}</StatusBadge>
                    </TableCell>
                    <TableCell>{stageOf(g.status)}</TableCell>
                    <TableCell>
                      <Age iso={g.updated_at} />
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

export function NewGoalPage() {
  const navigate = useNavigate()
  const [project, setProject] = useState("")
  const [title, setTitle] = useState("")
  const [body, setBody] = useState("")
  const [platforms, setPlatforms] = useState("")
  const [errors, setErrors] = useState<{ project?: string; title?: string; form?: string }>({})
  const [busy, setBusy] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const next: typeof errors = {}
    if (!project) next.project = "Choose a project."
    if (!title.trim()) next.title = "Give the goal a title."
    setErrors(next)
    if (next.project || next.title) return
    setBusy(true)
    try {
      const goal = await api.createGoal({ project, title: title.trim(), body, platforms: splitList(platforms) })
      await navigate({ to: "/goals/$ref", params: { ref: goalRef(goal.number) } })
    } catch (err) {
      setErrors({ form: err instanceof ApiError ? err.message : "Could not create the goal." })
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <PageHeader title="New goal" />
      <form onSubmit={(e) => void submit(e)} className="max-w-xl space-y-4" noValidate>
        <Field label="Project" error={errors.project}>
          {(p) => <ProjectSelect {...p} className="w-full" value={project} onChange={setProject} />}
        </Field>
        <Field label="Title" error={errors.title}>
          {(p) => <Input {...p} value={title} onChange={(e) => setTitle(e.target.value)} />}
        </Field>
        <Field label="Description" hint="What do you want, and why? Markdown works.">
          {(p) => <Textarea {...p} rows={6} value={body} onChange={(e) => setBody(e.target.value)} />}
        </Field>
        <Field label="Platforms" hint="Optional, separated by commas: web, ios, android.">
          {(p) => <Input {...p} value={platforms} onChange={(e) => setPlatforms(e.target.value)} />}
        </Field>
        {errors.form ? (
          <p role="alert" className="text-sm text-destructive">
            {errors.form}
          </p>
        ) : null}
        <Button type="submit" disabled={busy}>
          {busy ? "Creating…" : "Create goal"}
        </Button>
      </form>
    </>
  )
}
