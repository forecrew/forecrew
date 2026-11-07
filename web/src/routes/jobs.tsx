import { Link, useNavigate, useSearch } from "@tanstack/react-router"
import { useJobs, useProjects } from "@/api/queries"
import { JOB_STATUSES, type JobStatus } from "@/api/types"
import { Age } from "@/components/Age"
import { CancelJob, cancelable } from "@/components/CancelJob"
import { Async } from "@/components/Async"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { cost, jobRef, jobTone } from "@/lib/format"

export function JobsPage() {
  const { project = "", status = "" } = useSearch({ from: "/app/jobs" })
  const navigate = useNavigate({ from: "/jobs" })
  const jobs = useJobs(project, status as JobStatus | "")
  const projects = useProjects()
  const names = new Map(projects.data?.map((p) => [p.id, p.name]))
  const setFilter = (next: { status?: string }) =>
    void navigate({
      search: (prev) => ({ ...prev, ...Object.fromEntries(Object.entries(next).map(([k, v]) => [k, v || undefined])) }),
    })

  return (
    <>
      <PageHeader title="Jobs" />
      <div className="mb-4 flex flex-wrap gap-4">
        <div className="space-y-1.5">
          <Label htmlFor="job-status">State</Label>
          <NativeSelect id="job-status" className="w-full sm:w-48" value={status} onChange={(e) => setFilter({ status: e.target.value })}>
            <NativeSelectOption value="">All states</NativeSelectOption>
            {JOB_STATUSES.map((s) => (
              <NativeSelectOption key={s} value={s}>
                {s}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        </div>
      </div>
      <Async query={jobs} isEmpty={(d) => d.length === 0} empty="No jobs match.">
        {(items) => (
          <TableWrap label="Jobs">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Job</TableHead>
                  <TableHead>Project</TableHead>
                  <TableHead>State</TableHead>
                  <TableHead>Runtime</TableHead>
                  <TableHead className="text-right">Cost</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead>Actions</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((j) => (
                  <TableRow key={j.id}>
                    <TableCell>
                      <Link to="/jobs/$ref" params={{ ref: jobRef(j.number) }} className="inline-flex items-center gap-2 underline-offset-2 hover:underline">
                        <span className="font-mono">{jobRef(j.number)}</span> {j.role}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono">{names.get(j.project_id) ?? ""}</TableCell>
                    <TableCell>
                      <StatusBadge tone={jobTone(j.status)}>{j.status}</StatusBadge>
                    </TableCell>
                    <TableCell className="font-mono">{j.runtime}</TableCell>
                    <TableCell className="text-right">{cost(j.cost_usd)}</TableCell>
                    <TableCell>
                      <Age iso={j.created_at} ago />
                    </TableCell>
                    <TableCell>{cancelable(j.status) ? <CancelJob jobRef={jobRef(j.number)} label="Cancel" ariaLabel={`Cancel ${jobRef(j.number)}`} projectId={j.project_id} /> : null}</TableCell>
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
