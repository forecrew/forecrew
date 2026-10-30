import { act, screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { errors, fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

describe("job list", () => {
  test("lists jobs with project, state, runtime and cost", async () => {
    await renderApp("/jobs")
    const row = (await screen.findByRole("link", { name: /JOB-41/ })).closest("tr")!
    expect(within(row).getByText("running")).toBeInTheDocument()
    expect(within(row).getByText("app")).toBeInTheDocument()
    expect(within(row).getByText("claude-code")).toBeInTheDocument()
    expect(within(row).getByText("$0.12")).toBeInTheDocument()
    expect(within(row).getByText("2 h ago")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: /JOB-41 developer/ })).toHaveAttribute("href", "/jobs/JOB-41")
  })

  test("filters by project and state", async () => {
    const seen: string[] = []
    server.use(
      http.get("/api/v1/jobs", ({ request }) => {
        seen.push(new URL(request.url).search)
        return HttpResponse.json(fx.jobs)
      }),
    )
    const { user } = await renderApp("/jobs?status=queued")
    expect(await screen.findByLabelText("State")).toHaveValue("queued")
    await user.selectOptions(await screen.findByLabelText("Switch project"), "app")
    await waitFor(() => expect(seen).toContain("?project=app&status=queued"))
    await user.selectOptions(screen.getByLabelText("State"), "")
    await waitFor(() => expect(seen.at(-1)).toBe("?project=app"))
    expect(screen.queryByLabelText("Project")).not.toBeInTheDocument() // the sidebar switcher is the only project filter
  })

  test("shows empty and error states", async () => {
    server.use(http.get("/api/v1/jobs", () => HttpResponse.json(fx.jobsEmpty)))
    const { unmount } = await renderApp("/jobs")
    expect(await screen.findByText("No jobs match.")).toBeInTheDocument()
    unmount()
    server.use(http.get("/api/v1/jobs", errors.invalidInput))
    await renderApp("/jobs")
    expect(await screen.findByText("Could not load this")).toBeInTheDocument()
  })
})

describe("job page", () => {
  test("shows the job and its log", async () => {
    await renderApp("/jobs/JOB-41")
    expect(await screen.findByRole("heading", { name: "JOB-41 · developer" })).toBeInTheDocument()
    expect(screen.getByText("running")).toBeInTheDocument()
    expect(screen.getByText("1 of 3")).toBeInTheDocument()
    expect(screen.getByText("build-box-1")).toBeInTheDocument()
    expect(screen.getByText("claude-sonnet-5")).toBeInTheDocument()
    expect(screen.getByText("acme/app @ agent/task-12")).toBeInTheDocument()
    expect(screen.getByText("$0.12")).toBeInTheDocument()

    const log = await screen.findByRole("log", { name: "Job log" })
    await waitFor(() => expect(log).toHaveTextContent("I will start with the mailer."))
    expect(log).toHaveTextContent("001 status cloning")
    expect(log).toHaveTextContent('Bash {"command":"go test ./..."}')
    expect(log).toHaveTextContent("Bash returned: ok")
  })

  test("links back to the jobs list and puts the log beside the state and cost", async () => {
    await renderApp("/jobs/JOB-41")
    const main = await screen.findByRole("main")
    expect(within(main).getByRole("link", { name: "Jobs" })).toHaveAttribute("href", "/jobs")
    const log = await screen.findByRole("log", { name: "Job log" })
    const cost = screen.getByText("Cost")
    expect(log.compareDocumentPosition(cost) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
  })

  test("shows the state next to the title and goal, worker and model under it", async () => {
    await renderApp("/jobs/JOB-41")
    const title = await screen.findByRole("heading", { level: 1, name: "JOB-41 · developer" })
    expect(within(title.parentElement as HTMLElement).getByText("running")).toBeInTheDocument()
    const meta = (title.parentElement?.nextElementSibling ?? document.body) as HTMLElement
    expect(await within(meta).findByRole("link", { name: /GOAL-3/ })).toBeInTheDocument()
    expect(meta).toHaveTextContent(/worker .* · model /)
    expect(screen.queryByText("State", { selector: "dt" })).not.toBeInTheDocument()
  })

  test("shows why a queued job waits", async () => {
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json(fx.jobQueued)), http.get("/api/v1/jobs/:ref/logs", () => HttpResponse.json(fx.jobLogsEmpty)))
    await renderApp("/jobs/JOB-42")
    expect(await screen.findByText(/no matching worker/)).toBeInTheDocument()
    expect(screen.getByText("queued")).toBeInTheDocument()
    expect(screen.getByText("none")).toBeInTheDocument() // no worker yet
    expect(await screen.findByText("No events.")).toBeInTheDocument()
  })

  test("shows a failed job with its reason and end time", async () => {
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json(fx.jobFailed)))
    await renderApp("/jobs/JOB-43")
    expect(await screen.findByText("attempts exhausted: lease expired")).toBeInTheDocument()
    expect(screen.getByText("failed")).toBeInTheDocument()
    expect(screen.getByText("Finished")).toBeInTheDocument()
  })

  test("waits for the first event of a running job", async () => {
    server.use(http.get("/api/v1/jobs/:ref/logs", () => HttpResponse.json(fx.jobLogsEmpty)))
    await renderApp("/jobs/JOB-41")
    expect(await screen.findByText("Waiting for the first event…")).toBeInTheDocument()
  })

  test("shows loading and error states", async () => {
    server.use(http.get("/api/v1/jobs/:ref", errors.notFound))
    await renderApp("/jobs/JOB-99")
    expect(screen.getByRole("status")).toBeInTheDocument()
    expect(await screen.findByText(/GOAL-99: not found/)).toBeInTheDocument()
  })

  test("shows a log that cannot be loaded", async () => {
    server.use(http.get("/api/v1/jobs/:ref/logs", errors.invalidInput))
    await renderApp("/jobs/JOB-41")
    expect(await screen.findByText(/Could not load the log/)).toBeInTheDocument()
  })

  test("agent output is shown as text", async () => {
    const { container } = await renderApp("/jobs/JOB-41")
    const log = await screen.findByRole("log")
    await waitFor(() => expect(log).toHaveTextContent("<img src=x onerror=alert(1)> done"))
    expect(container.querySelector("img, script")).toBeNull()
  })

  test("renders every kind of event", async () => {
    server.use(http.get("/api/v1/jobs/:ref/logs", () => HttpResponse.json(fx.jobLogsAllKinds)))
    await renderApp("/jobs/JOB-41")
    const log = await screen.findByRole("log")
    await waitFor(() => expect(log).toHaveTextContent("it broke"))
    expect(log).toHaveTextContent("usage tokens: 5 input, 2 output · $0.01")
    expect(log).toHaveTextContent("Bash failed: boom")
    expect(log).toHaveTextContent("tool {}")
    expect(log).toHaveTextContent("tool returned:")
  })
})

describe("job page details", () => {
  const jobWith = (extra: object) => server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json({ ...fx.job, ...extra })))

  test("shows the needs, the token totals and the goal link", async () => {
    await renderApp("/jobs/JOB-41")
    const needs = (await screen.findByText("Needs")).nextElementSibling as HTMLElement
    expect(within(needs).getByText("go")).toBeInTheDocument()
    expect(within(needs).getByText("linux")).toBeInTheDocument()
    expect(screen.getByText("1,200 input, 340 output, 0 cache read, 0 cache write")).toBeInTheDocument()
    expect(await screen.findByRole("link", { name: "GOAL-3" })).toHaveAttribute("href", "/goals/GOAL-3")
    expect(screen.queryByText("Task")).not.toBeInTheDocument()
  })

  test("shows the needs next to the reason a queued job waits", async () => {
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json(fx.jobQueued)), http.get("/api/v1/jobs/:ref/logs", () => HttpResponse.json(fx.jobLogsEmpty)))
    await renderApp("/jobs/JOB-42")
    await screen.findByText(/no matching worker/)
    const labels = Array.from(document.querySelectorAll("dt")).map((t) => t.textContent)
    expect(labels.indexOf("Needs")).toBe(labels.indexOf("Reason") + 1)
  })

  test("shows when the lease of a running job expires", async () => {
    jobWith({ lease_expires_at: "2026-03-01T12:05:00Z" })
    await renderApp("/jobs/JOB-41")
    expect((await screen.findByText("Lease expires")).nextElementSibling).toHaveTextContent("in 5 min")
  })

  test("shows when a queued job retries", async () => {
    server.use(http.get("/api/v1/jobs/:ref/logs", () => HttpResponse.json(fx.jobLogsEmpty)))
    jobWith({ status: "queued", worker_id: undefined, run_after: "2026-03-01T12:03:00Z" })
    const { unmount } = await renderApp("/jobs/JOB-41")
    expect((await screen.findByText("Retry")).nextElementSibling).toHaveTextContent("Waiting to retry in 3 min")
    unmount()
    jobWith({ status: "queued", worker_id: undefined, run_after: "2026-03-01T11:00:00Z" })
    await renderApp("/jobs/JOB-41")
    expect((await screen.findByText("Retry")).nextElementSibling).toHaveTextContent("Waiting to retry now")
  })

  test("shows the output as formatted JSON in a folded block", async () => {
    jobWith({ status: "succeeded", output: { verdict: "approve", notes: ["ok"] } })
    const { user } = await renderApp("/jobs/JOB-41")
    const summary = await screen.findByText("Output")
    const details = summary.closest("details")!
    expect(details).not.toHaveAttribute("open")
    await user.click(summary)
    expect(details).toHaveAttribute("open")
    expect(screen.getByLabelText("Output as JSON").textContent).toBe(JSON.stringify({ verdict: "approve", notes: ["ok"] }, null, 2))
  })

  test("has no output block without output", async () => {
    await renderApp("/jobs/JOB-41")
    await screen.findByText("claude-sonnet-5")
    expect(screen.queryByText("Output")).not.toBeInTheDocument()
  })

  test("links the head commit on GitHub when the repo is known", async () => {
    jobWith({ status: "succeeded", head_sha: "abc1234def5678" })
    const { unmount } = await renderApp("/jobs/JOB-41")
    expect(await screen.findByRole("link", { name: /abc1234/ })).toHaveAttribute("href", "https://github.com/acme/app/commit/abc1234def5678")
    unmount()
    jobWith({ status: "succeeded", head_sha: "abc1234def5678", spec: { ...fx.job.spec, repo: "" } })
    await renderApp("/jobs/JOB-41")
    expect(await screen.findByText("abc1234")).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: /abc1234/ })).not.toBeInTheDocument()
  })

  test("links the task of the job to its issue", async () => {
    jobWith({ task_id: fx.goal.tasks[0]!.id })
    await renderApp("/jobs/JOB-41")
    expect(await screen.findByRole("link", { name: /TASK-12/ })).toHaveAttribute("href", "https://github.com/acme/app/issues/6")
  })

  test("copes with a job whose task is not in the goal", async () => {
    jobWith({ task_id: "018f0000-0000-7000-8000-0000000009ff" })
    await renderApp("/jobs/JOB-41")
    expect(await screen.findByText("unknown")).toBeInTheDocument()
  })

  test("copes with a job without goal and a goal that cannot be read", async () => {
    jobWith({ goal_id: undefined })
    const { unmount } = await renderApp("/jobs/JOB-41")
    await screen.findByText("claude-sonnet-5")
    expect(screen.queryByText("Goal")).not.toBeInTheDocument()
    unmount()
    jobWith({})
    server.use(http.get("/api/v1/goals/:ref", errors.notFound))
    await renderApp("/jobs/JOB-41")
    expect(await screen.findByRole("link", { name: "Goal" })).toHaveAttribute("href", `/goals/${fx.job.goal_id}`)
  })
})

describe("following the log", () => {
  /** A log API with the events fixture split in two, asking for the cursor of each call. */
  function growingLog() {
    const all = fx.jobLogs.events
    const cursors: string[] = []
    let available = 2
    server.use(
      http.get("/api/v1/jobs/:ref/logs", ({ request }) => {
        const after = new URL(request.url).searchParams.get("after_seq") ?? ""
        cursors.push(after)
        const events = all.filter((e) => e.seq > Number(after) && e.seq <= available)
        return HttpResponse.json({ events, next_seq: events.at(-1)?.seq ?? Number(after) })
      }),
    )
    return {
      cursors,
      grow: (n: number) => {
        available = n
      },
    }
  }

  test("polls from the last cursor every 2 seconds while the job runs and appends new events", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval", "setTimeout", "clearTimeout"], shouldAdvanceTime: true })
    vi.setSystemTime(new Date("2026-03-01T12:00:00Z"))
    const { cursors, grow } = growingLog()
    await renderApp("/jobs/JOB-41")
    const log = await screen.findByRole("log")
    await waitFor(() => expect(log).toHaveTextContent("I will start with the mailer."))
    expect(log).not.toHaveTextContent("go test")
    expect(cursors).toEqual([""]) // no cursor yet

    grow(5)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100)
    })
    await waitFor(() => expect(log).toHaveTextContent("Bash returned: ok"))
    expect(log).toHaveTextContent("001 status cloning") // earlier events are kept
    expect(cursors).toContain("2")
    expect(screen.getAllByText(/status/)).toHaveLength(1) // no duplicates
  })

  test("scrolls to the newest event while following and stops when the reader scrolls up", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval", "setTimeout", "clearTimeout"], shouldAdvanceTime: true })
    vi.setSystemTime(new Date("2026-03-01T12:00:00Z"))
    const { grow } = growingLog()
    const { user } = await renderApp("/jobs/JOB-41")
    const log = await screen.findByRole("log")
    await waitFor(() => expect(log).toHaveTextContent("mailer"))
    let top = 0
    Object.defineProperty(log, "scrollHeight", { configurable: true, get: () => 1000 })
    Object.defineProperty(log, "clientHeight", { configurable: true, get: () => 200 })
    Object.defineProperty(log, "scrollTop", { configurable: true, get: () => top, set: (v: number) => (top = v) })

    grow(4)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100)
    })
    await waitFor(() => expect(log).toHaveTextContent("go test"))
    expect(top).toBe(1000) // followed

    top = 100 // the reader scrolls up
    act(() => {
      log.dispatchEvent(new Event("scroll", { bubbles: true }))
    })
    grow(5)
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100)
    })
    await waitFor(() => expect(log).toHaveTextContent("Bash returned: ok"))
    expect(top).toBe(100) // not pulled away
    await user.click(screen.getByRole("button", { name: "Follow new events" }))
    expect(top).toBe(1000)
    expect(screen.queryByRole("button", { name: "Follow new events" })).not.toBeInTheDocument()
  })

  test("a finished job is not polled and its log is read once more", async () => {
    vi.useFakeTimers({ toFake: ["Date", "setInterval", "clearInterval", "setTimeout", "clearTimeout"], shouldAdvanceTime: true })
    vi.setSystemTime(new Date("2026-03-01T12:00:00Z"))
    const { cursors } = growingLog()
    server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json({ ...fx.job, status: "succeeded" })))
    await renderApp("/jobs/JOB-41")
    await screen.findByRole("log")
    await waitFor(() => expect(cursors.length).toBeGreaterThan(0))
    const before = cursors.length
    await act(async () => {
      await vi.advanceTimersByTimeAsync(6000)
    })
    expect(cursors.length).toBe(before)
  })
})
