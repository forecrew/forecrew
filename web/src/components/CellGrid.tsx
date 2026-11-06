import { Link } from "@tanstack/react-router"
import type { QualityCell } from "@/api/types"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { TableWrap } from "@/components/TableWrap"
import { cn } from "cn"
import { cellLabel, countsText, envLabel } from "@/lib/format"

const TEXT = { good: "text-success", bad: "text-destructive", neutral: "" } as const
const toneOf = (s: string) => (s === "passed" ? TEXT.good : s === "failed" || s === "errored" ? TEXT.bad : TEXT.neutral)

const uniq = (xs: string[]) => [...new Set(xs)].sort()

/** Totals of a list of cells. */
export function totals(cells: QualityCell[]) {
  return cells.reduce(
    (t, c) => ({ passed: t.passed + c.counts.passed, failed: t.failed + c.counts.failed + c.counts.errored, skipped: t.skipped + c.counts.skipped }),
    { passed: 0, failed: 0, skipped: 0 },
  )
}

/**
 * Suites by environments. Each result is a link to its job and says its
 * status and counts in words, so the colour is never the only signal. A suite
 * run in several matrix cells of an environment lists each cell.
 */
export function CellGrid({ cells, label, flaky = {} }: { cells: QualityCell[]; label: string; flaky?: Record<string, number> }) {
  const suites = uniq(cells.map((c) => c.suite))
  const envs = uniq(cells.map((c) => c.environment))
  return (
    <TableWrap label={label}>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Suite</TableHead>
            {envs.map((e) => (
              <TableHead key={e}>
                <span className="font-mono">{envLabel(e)}</span>
                {flaky[e] ? <span className="ml-2 font-normal text-warning">{flaky[e]} flaky</span> : null}
              </TableHead>
            ))}
          </TableRow>
        </TableHeader>
        <TableBody>
          {suites.map((suite) => (
            <TableRow key={suite}>
              <TableCell className="font-mono">{suite}</TableCell>
              {envs.map((env) => {
                const here = cells.filter((c) => c.suite === suite && c.environment === env)
                return (
                  <TableCell key={env} className="align-top whitespace-normal">
                    {here.length === 0 ? (
                      <>
                        <span aria-hidden="true">-</span>
                        <span className="sr-only">no result</span>
                      </>
                    ) : (
                      <ul className="space-y-1">
                        {here.map((c) => (
                          <li key={c.job}>
                            <Link to="/jobs/$ref" params={{ ref: c.job }} className={cn("underline underline-offset-2", toneOf(c.status))}>
                              {cellLabel(c.cell) ? `${cellLabel(c.cell)}: ` : ""}
                              {c.status}, {countsText(c.counts)}
                              <span className="sr-only"> ({c.job})</span>
                            </Link>
                          </li>
                        ))}
                      </ul>
                    )}
                  </TableCell>
                )
              })}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </TableWrap>
  )
}

/** The failed and errored tests of the cells, as text: names and messages come from the repo's tests. */
export function FailureList({ cells, label }: { cells: QualityCell[]; label: string }) {
  const failing = cells.filter((c) => c.failures.length > 0)
  if (failing.length === 0) return null
  return (
    <div className="space-y-1 text-sm">
      <p className="text-muted-foreground">Failing tests:</p>
      <ul aria-label={label} className="space-y-1">
        {failing.flatMap((c) =>
          c.failures.map((f, i) => (
            <li key={`${c.job}-${c.suite}-${i}`} className="break-words">
              <span className="font-medium text-destructive">{f.status}</span> <span className="font-mono">{[f.classname, f.name].filter(Boolean).join(" ")}</span>{" "}
              <span className="text-muted-foreground">
                in {c.suite}, {envLabel(c.environment)}
                {f.message ? `: ${f.message.split("\n")[0]}` : ""}
              </span>
            </li>
          )),
        )}
        {failing.some((c) => c.failures_truncated) ? <li className="text-muted-foreground">More failures than listed.</li> : null}
      </ul>
    </div>
  )
}
