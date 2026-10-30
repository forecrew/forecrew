import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

/** Records the cancel calls and answers with the recorded job. */
function recordCancels() {
  const refs: string[] = []
  server.use(
    http.post("/api/v1/jobs/:ref/cancel", ({ params }) => {
      refs.push(String(params.ref))
      return HttpResponse.json(fx.jobCancel)
    }),
  )
  return refs
}

describe("cancel a job", () => {
  test("from the job page: confirms, cancels and refreshes the job, the job lists and the gates", async () => {
    const refs = recordCancels()
    const fetched = { job: 0, gates: 0 }
    server.use(
      http.get("/api/v1/jobs/:ref", () => {
        fetched.job++
        return HttpResponse.json(fetched.job > 1 ? fx.jobCancel : fx.job)
      }),
      http.get("/api/v1/gates", () => {
        fetched.gates++
        return HttpResponse.json(fx.gates)
      }),
    )
    const { user, queryClient } = await renderApp("/jobs/JOB-41")
    // lists that are not on screen are marked stale, and read again when opened
    queryClient.setQueryData(["jobs", "", ""], fx.jobs)
    queryClient.setQueryData(["goal-jobs", "GOAL-3"], fx.jobs)
    await user.click(await screen.findByRole("button", { name: "Cancel job" }))
    const dialog = await screen.findByRole("dialog", { name: "Cancel JOB-41?" })
    expect(dialog).toHaveTextContent("stops on its worker within about 30 seconds")
    expect(dialog).toHaveTextContent("failure path")
    expect(refs).toEqual([])
    const before = { ...fetched }

    await user.click(within(dialog).getByRole("button", { name: "Cancel JOB-41" }))
    await waitFor(() => expect(refs).toEqual(["JOB-41"]))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    await waitFor(() => expect(fetched.job).toBeGreaterThan(before.job))
    await waitFor(() => expect(queryClient.getQueryState(["jobs", "", ""])?.isInvalidated).toBe(true))
    expect(queryClient.getQueryState(["goal-jobs", "GOAL-3"])?.isInvalidated).toBe(true)
    expect(fetched.gates).toBeGreaterThan(before.gates)
    // the job is canceled now: no more button
    await waitFor(() => expect(screen.queryByRole("button", { name: "Cancel job" })).not.toBeInTheDocument())
  })

  test("keeping the job closes the dialog without a call", async () => {
    const refs = recordCancels()
    const { user } = await renderApp("/jobs/JOB-41")
    await user.click(await screen.findByRole("button", { name: "Cancel job" }))
    await user.click(await screen.findByRole("button", { name: "Keep job" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    expect(refs).toEqual([])
    expect(screen.getByRole("button", { name: "Cancel job" })).toHaveFocus()
  })

  test("Escape closes the dialog", async () => {
    const { user } = await renderApp("/jobs/JOB-41")
    await user.click(await screen.findByRole("button", { name: "Cancel job" }))
    await screen.findByRole("dialog")
    await user.keyboard("{Escape}")
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
  })

  test("shows the API message on a 409 and clears it when reopened", async () => {
    server.use(http.post("/api/v1/jobs/:ref/cancel", errors.cancelConflict))
    const { user } = await renderApp("/jobs/JOB-41")
    await user.click(await screen.findByRole("button", { name: "Cancel job" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel JOB-41" }))
    expect(await screen.findByRole("alert")).toHaveTextContent("job JOB-41 is succeeded: invalid state transition")
    expect(screen.getByRole("dialog")).toBeInTheDocument()
    await user.click(screen.getByRole("button", { name: "Keep job" }))
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
    await user.click(screen.getByRole("button", { name: "Cancel job" }))
    await screen.findByRole("dialog")
    expect(screen.queryByRole("alert")).not.toBeInTheDocument()
  })

  test("is offered for queued jobs and not for finished ones", async () => {
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json(fx.jobQueued)), http.get("/api/v1/jobs/:ref/logs", () => HttpResponse.json(fx.jobLogsEmpty)))
    const { unmount } = await renderApp("/jobs/JOB-42")
    expect(await screen.findByRole("button", { name: "Cancel job" })).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json(fx.jobFailed)))
    await renderApp("/jobs/JOB-43")
    await screen.findByText("failed")
    expect(screen.queryByRole("button", { name: /Cancel/ })).not.toBeInTheDocument()
  })

  test("from the jobs page: only queued and running rows have the action", async () => {
    const refs = recordCancels()
    const { user } = await renderApp("/jobs")
    const running = (await screen.findByRole("link", { name: /JOB-41/ })).closest("tr")!
    const queued = screen.getByRole("link", { name: /JOB-42/ }).closest("tr")!
    const done = screen.getByRole("link", { name: /JOB-40/ }).closest("tr")!
    expect(within(queued).getByRole("button", { name: "Cancel JOB-42" })).toBeInTheDocument()
    expect(within(done).queryByRole("button")).not.toBeInTheDocument()

    within(running).getByRole("button", { name: "Cancel JOB-41" }).focus()
    await user.keyboard("{Enter}")
    const dialog = await screen.findByRole("dialog", { name: "Cancel JOB-41?" })
    await user.click(within(dialog).getByRole("button", { name: "Cancel JOB-41" }))
    await waitFor(() => expect(refs).toEqual(["JOB-41"]))
  })

  test("from the jobs table of a goal", async () => {
    const refs = recordCancels()
    const { user } = await renderApp("/goals/GOAL-3")
    const jobs = (await screen.findByRole("heading", { name: "Jobs" })).closest("section")!
    const row = (await within(jobs).findByRole("link", { name: /JOB-42/ })).closest("tr")!
    await user.click(within(row).getByRole("button", { name: "Cancel JOB-42" }))
    await user.click(within(await screen.findByRole("dialog")).getByRole("button", { name: "Cancel JOB-42" }))
    await waitFor(() => expect(refs).toEqual(["JOB-42"]))
    expect(within(jobs).queryByRole("button", { name: "Cancel JOB-40" })).not.toBeInTheDocument()
  })
})
