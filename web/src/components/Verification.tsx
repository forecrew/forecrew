import { ApiError, artifactUrl } from "@/api/client"
import { useVerification } from "@/api/queries"
import type { VerificationInfo } from "@/api/types"
import { StatusBadge } from "@/components/StatusBadge"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { TableWrap } from "@/components/TableWrap"
import { resultTone, verificationTone } from "@/lib/format"

/** The criteria that have a result, and the untested ones. */
export function coverage(report: VerificationInfo["report"]) {
  const all = Object.entries(report.criteria)
  const untested = all.filter(([, c]) => c.status === "untested" && !c.untestable).map(([id]) => id)
  return { total: all.length, covered: all.filter(([, c]) => c.status !== "untested").length, untested }
}

/**
 * One verification report: status, why the merge is blocked, the environments
 * with their tool versions, the traceability table (criterion, tests, result),
 * flaky tests and the link to the rendered report. Test names come from the
 * repo and are shown as text.
 */
export function VerificationPanel({ info, title }: { info: VerificationInfo; title: string }) {
  const r = info.report
  const cov = coverage(r)
  const envs = Object.entries(r.environments)
  const criteria = Object.entries(r.criteria)
  return (
    <section aria-label={title} className="space-y-3 rounded-lg border p-3 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <h3 className="font-medium">Verification</h3>
        <StatusBadge tone={verificationTone(info.status)}>{info.status}</StatusBadge>
        {info.current ? null : <StatusBadge tone="attention">stale: not the latest commit</StatusBadge>}
        <span className="font-mono text-xs text-muted-foreground">{info.sha.slice(0, 7)}</span>
        {info.artifact_id ? (
          <a href={artifactUrl(info.artifact_id)} download className="ml-auto text-primary underline underline-offset-2">
            Download report
          </a>
        ) : null}
      </div>
      {r.blocking.length ? (
        <ul aria-label="Blocking reasons" className="list-disc space-y-0.5 pl-5 text-destructive">
          {r.blocking.map((b) => (
            <li key={b}>{b}</li>
          ))}
        </ul>
      ) : null}
      {envs.length ? (
        <p>
          <span className="text-muted-foreground">Environments: </span>
          {envs.map(([name, e], i) => (
            <span key={name}>
              {i ? "; " : ""}
              <span className="font-mono">{name || "host"}</span>
              {Object.keys(e.versions).length ? ` (${Object.entries(e.versions).map(([k, v]) => `${k} ${v}`).join(", ")})` : ""}
            </span>
          ))}
        </p>
      ) : null}
      {criteria.length ? (
        <div className="space-y-1">
          <p className="text-muted-foreground">
            Criteria: {cov.covered} of {cov.total} covered{cov.untested.length ? `, untested: ${cov.untested.join(", ")}` : ""}
          </p>
          <TableWrap label={`${title} criteria`}>
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Criterion</TableHead>
                  <TableHead>Result</TableHead>
                  <TableHead>Tests</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {criteria.map(([id, c]) => (
                  <TableRow key={id}>
                    <TableCell className="font-mono">{id}</TableCell>
                    <TableCell>
                      <StatusBadge tone={c.status === "untested" ? "attention" : resultTone(c.status)}>{c.status}</StatusBadge>
                    </TableCell>
                    <TableCell className="whitespace-normal">
                      {Object.keys(c.tests).length ? (
                        <ul>
                          {Object.entries(c.tests).map(([test, result]) => (
                            <li key={test}>
                              <span className="font-mono">{test}</span> <span className="text-muted-foreground">{result}</span>
                            </li>
                          ))}
                        </ul>
                      ) : (
                        <span className="text-muted-foreground">{c.untestable ? `untestable: ${c.untestable}` : "no tests"}</span>
                      )}
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        </div>
      ) : null}
      {r.flaky.length ? (
        <div>
          <p className="text-muted-foreground">Flaky tests (failed, then passed on rerun):</p>
          <ul aria-label="Flaky tests" className="list-disc pl-5">
            {r.flaky.map((f) => (
              <li key={f.test}>
                <span className="font-mono">{f.test}</span> in {f.environments.map((e) => e || "host").join(", ")}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </section>
  )
}

/**
 * The verification of a change fetched by number (with project), PR URL or
 * task ref. Nothing is shown while it loads or when the change has no report
 * yet (a 404); another error is said in words.
 */
export function VerificationSection({ change, project, title }: { change: string; project?: string; title: string }) {
  const q = useVerification(change, project)
  if (q.isError && !(q.error instanceof ApiError && q.error.status === 404)) {
    return (
      <p role="alert" className="text-sm text-destructive">
        Could not load the verification: {q.error.message}
      </p>
    )
  }
  if (!q.data) return null
  return <VerificationPanel info={q.data} title={title} />
}
