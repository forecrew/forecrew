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
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { projectTone } from "@/lib/format"

export function ProjectsPage() {
  const projects = useProjects()
  return (
    <>
      <PageHeader title="Projects">
        <Button asChild>
          <Link to="/projects/new">Onboard repository</Link>
        </Button>
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
                      <Link to="/projects/$name" params={{ name: p.name }} className="inline-flex min-h-11 items-center underline-offset-2 hover:underline">
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
      const { project } = await api.onboardProject({ repo: value })
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
        <Button type="submit" disabled={busy}>
          {busy ? "Onboarding…" : "Onboard repository"}
        </Button>
      </form>
    </>
  )
}
