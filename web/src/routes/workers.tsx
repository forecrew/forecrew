import { useWorkers } from "@/api/queries"
import { Age } from "@/components/Age"
import { Async } from "@/components/Async"
import { PageHeader } from "@/components/PageHeader"
import { StatusBadge } from "@/components/StatusBadge"
import { TableWrap } from "@/components/TableWrap"
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table"
import { Link } from "@tanstack/react-router"

function AddWorker() {
  return (
    <section aria-labelledby="add-worker" className="mt-8 rounded-xl border bg-card p-4">
      <h2 id="add-worker" className="mb-2 text-lg font-semibold">
        Add a worker
      </h2>
      <ol className="list-decimal space-y-3 pl-5 text-sm">
        <li>
          On the server, register it. The token is shown once.
          <pre tabIndex={0} className="mt-1 overflow-x-auto rounded-lg bg-muted p-3 font-mono text-xs">forecrew worker add build-box-1</pre>
        </li>
        <li>
          On the worker machine, run the install script as root with that token in <code className="font-mono">FORECREW_WORKER_TOKEN</code>.
          <pre tabIndex={0} className="mt-1 overflow-x-auto rounded-lg bg-muted p-3 font-mono text-xs">
            {`curl -fsSLO https://raw.githubusercontent.com/forecrew/forecrew/main/deploy/worker/install.sh
sudo --preserve-env=FORECREW_WORKER_TOKEN sh install.sh \\
  --server ${window.location.origin} --name build-box-1 --labels go,node,docker`}
          </pre>
        </li>
      </ol>
      <p className="mt-3 text-sm text-muted-foreground">
        The full steps are in the add-worker runbook (docs/runbooks/add-worker.md).
      </p>
    </section>
  )
}

export function WorkersPage() {
  const workers = useWorkers()
  return (
    <>
      <PageHeader title="Workers" />
      <Async query={workers} isEmpty={(d) => d.length === 0} empty="No workers yet. Add one below.">
        {(items) => (
          <TableWrap label="Workers">
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Worker</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Labels</TableHead>
                  <TableHead>Version</TableHead>
                  <TableHead>Running</TableHead>
                  <TableHead>Last seen</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {items.map((w) => (
                  <TableRow key={w.id}>
                    <TableCell className="font-mono">{w.name}</TableCell>
                    <TableCell>
                      <StatusBadge tone={w.online ? "good" : "bad"}>{w.online ? "online" : "offline"}</StatusBadge>
                    </TableCell>
                    <TableCell>
                      <ul className="flex flex-wrap gap-1" aria-label={`Labels of ${w.name}`}>
                        {w.labels.map((l) => (
                          <li key={l}>
                            <StatusBadge className="font-mono">{l}</StatusBadge>
                          </li>
                        ))}
                      </ul>
                    </TableCell>
                    <TableCell className="font-mono">{w.version}</TableCell>
                    <TableCell>
                      {w.running_jobs.length ? (
                        <ul className="space-y-1">
                          {w.running_jobs.map((r) => (
                            <li key={r}>
                              <Link to="/jobs/$ref" params={{ ref: r }} className="inline-flex min-h-11 items-center font-mono underline underline-offset-2">
                                {r}
                              </Link>
                            </li>
                          ))}
                        </ul>
                      ) : (
                        <span className="text-muted-foreground">idle</span>
                      )}
                    </TableCell>
                    <TableCell>
                      <Age iso={w.last_seen_at} ago />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          </TableWrap>
        )}
      </Async>
      <AddWorker />
    </>
  )
}
