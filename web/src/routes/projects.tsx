import { Link, useNavigate } from "@tanstack/react-router"
import { useState } from "react"
import { ApiError, api } from "@/api/client"
import { useProjects } from "@/api/queries"
import { Async } from "@/components/Async"
import { ExternalLink } from "@/components/ExternalLink"
import { Field } from "@/components/Field"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { cn } from "cn"
import { Button, buttonVariants } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { projectTone } from "@/lib/format"

export function ProjectsPage() {
  const projects = useProjects()
  return (
    <>
      <PageHeader title="Projects">
        <Link to="/projects/new" className={cn(buttonVariants())}>
          Onboard repository
        </Link>
      </PageHeader>
      <Async query={projects} isEmpty={(d) => d.length === 0} empty="No projects yet. Onboard a repository to get started.">
        {(items) => (
          <TableWrap label="Projects">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Project</TableHead>
                  <TableHead>Repository</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((p) => (
                  <TableRow key={p.id}>
                    <TableCell>
                      <Link to="/projects/$name" params={{ name: p.name }} className="inline-flex items-center underline-offset-2 hover:underline">
                        {p.name}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">
                      <ExternalLink href={`https://github.com/${p.repo}`}>{p.repo}</ExternalLink>
                    </TableCell>
                    <TableCell>
                      <StatusBadge tone={projectTone(p.status)}>{p.status}</StatusBadge>
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

const REPO = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/

export function OnboardPage() {
  const navigate = useNavigate()
  const [repo, setRepo] = useState("")
  const [name, setName] = useState("")
  const [branch, setBranch] = useState("")
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    const value = repo.trim()
    if (!REPO.test(value)) {
      setError("Enter the repository as owner/name, for example acme/app.")
      return
    }
    setError("")
    setBusy(true)
    try {
      const { project } = await api.onboardProject({ repo: value, name: name.trim() || undefined, default_branch: branch.trim() || undefined })
      await navigate({ to: "/projects/$name", params: { name: project.name } })
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Could not onboard the repository.")
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <PageHeader title="Onboard repository" />
      <form onSubmit={(e) => void submit(e)} className="max-w-xl space-y-4" noValidate>
        <Field label="Repository" hint="The GitHub repository where the Forecrew app is installed, as owner/name." error={error}>
          {(p) => <Input {...p} className="font-mono" autoComplete="off" spellCheck={false} value={repo} onChange={(e) => setRepo(e.target.value)} />}
        </Field>
        <Field label="Name" hint="Optional. Defaults to the repository name.">
          {(p) => <Input {...p} className="font-mono" autoComplete="off" spellCheck={false} value={name} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Field label="Default branch" hint="Optional, for a repository that is new to Forecrew. Defaults to main.">
          {(p) => <Input {...p} className="font-mono" autoComplete="off" spellCheck={false} value={branch} onChange={(e) => setBranch(e.target.value)} />}
        </Field>
        <Button type="submit" disabled={busy}>
          {busy ? "Onboarding…" : "Onboard repository"}
        </Button>
      </form>
    </>
  )
}
