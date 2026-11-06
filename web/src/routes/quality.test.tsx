import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const use = (body: object) => server.use(http.get("/api/v1/quality", () => HttpResponse.json(body)))

describe("quality page", () => {
  test("is in the nav and says so when there are no projects", async () => {
    server.use(http.get("/api/v1/projects", () => HttpResponse.json(fx.projectsEmpty)))
    await renderApp("/quality")
    expect(await screen.findByRole("heading", { level: 1, name: "Quality" })).toBeInTheDocument()
    expect(await screen.findByText(/No projects yet/)).toBeInTheDocument()
    expect(within(screen.getByRole("navigation", { name: "Main" })).getByRole("link", { name: "Quality" })).toHaveAttribute("aria-current", "page")
  })

  test("empty: no baseline and no open pull requests", async () => {
    use(fx.qualityEmpty)
    await renderApp("/quality?project=app")
    expect(await screen.findByText("No baseline yet: no run outside a pull request.")).toBeInTheDocument()
    expect(screen.getByText("No open pull requests.")).toBeInTheDocument()
    expect(screen.queryByRole("article")).not.toBeInTheDocument()
  })

  test("baseline only: the grid with text for every result", async () => {
    use(fx.qualityBaselineOnly)
    await renderApp("/quality?project=app")
    const baseline = await screen.findByRole("article", { name: /Latest run outside a pull request/ })
    expect(within(baseline).getByText("0123456")).toBeInTheDocument()
    // flakiness is known only from a report
    expect(within(baseline).getByText("48 passed, 1 failed, 0 skipped")).toBeInTheDocument()
    const grid = within(baseline).getByRole("region", { name: /results table/ })
    expect(within(grid).getByRole("columnheader", { name: "linux" })).toBeInTheDocument()
    expect(within(grid).getByRole("link", { name: /go=1.26: passed, 40 passed/ })).toHaveAttribute("href", "/jobs/JOB-30")
    expect(within(grid).getByRole("link", { name: /go=1.26: failed, 8 passed, 1 failed/ })).toHaveAttribute("href", "/jobs/JOB-31")
    expect(within(baseline).getByRole("list", { name: /failing tests/ })).toHaveTextContent("web link expires in e2e, linux: known failure")
    expect(screen.getByText("No open pull requests.")).toBeInTheDocument()
    // no verification report on the baseline
    expect(within(baseline).queryByText("Verification")).not.toBeInTheDocument()
  })

  test("a pull request with failures: grid, failing tests, flaky and criteria", async () => {
    await renderApp("/quality?project=app")
    const pr = await screen.findByRole("article", { name: /Pull request #9/ })
    expect(within(pr).getByText("TASK-12")).toBeInTheDocument()
    expect(within(pr).getByRole("link", { name: /Open on GitHub/ })).toHaveAttribute("href", "https://github.com/acme/app/pull/9")
    expect(within(pr).getByText("48 passed, 2 failed, 1 skipped, 1 flaky")).toBeInTheDocument()

    // suites by environments: host is the empty environment
    const grid = within(pr).getByRole("region", { name: /results table/ })
    expect(within(grid).getAllByRole("columnheader").map((h) => h.textContent)).toEqual(["Suite", "host", "linux1 flaky", "macos"])
    const e2e = within(grid).getByText("e2e").closest("tr")!
    expect(within(e2e).getByRole("link", { name: /failed, 7 passed, 2 failed/ })).toHaveAttribute("href", "/jobs/JOB-45")
    expect(within(e2e).getAllByText("no result")).toHaveLength(2)
    // a cell the current report has no result for, not required
    const unit = within(grid).getByText("unit").closest("tr")!
    expect(within(unit).getAllByText("no result")).toHaveLength(2)
    const lint = within(grid).getByText("lint").closest("tr")!
    expect(within(lint).getByRole("link", { name: /passed, 1 passed/ })).toHaveAttribute("href", "/jobs/JOB-46")

    const failing = within(pr).getByRole("list", { name: /failing tests/ })
    expect(within(failing).getAllByRole("listitem")).toHaveLength(2)
    expect(failing).toHaveTextContent("failed web login redirects in e2e, linux: expected /home <b>got</b> /login")
    expect(failing).toHaveTextContent("errored web link expires in e2e, linux: timeout")
    expect(failing.querySelector("b")).toBeNull()

    const report = within(pr).getByRole("region", { name: /verification$/ })
    expect(within(report).getByRole("heading", { name: "Verification" }).parentElement).toHaveTextContent("failed")
    expect(within(report).getByRole("list", { name: "Flaky tests" })).toHaveTextContent("web::login redirects in linux")
    expect(within(report).getByText(/Criteria: 2 of 3 covered, untested: AC-3/)).toBeInTheDocument()
    expect(within(report).getByRole("list", { name: "Blocking reasons" })).toHaveTextContent("AC-3 has no test")
  })

  test("shows a required cell with no result in the grid, and not when the report is stale", async () => {
    const withMissing = structuredClone(fx.quality) as typeof fx.quality
    const v = withMissing.changes[0]!.verification!
    v.report.results[2]!.required = true
    use(withMissing)
    const { unmount } = await renderApp("/quality?project=app")
    const pr = await screen.findByRole("article", { name: /Pull request #9/ })
    const grid = within(pr).getByRole("region", { name: /results table/ })
    expect(within(grid).getByText("required, no result")).toHaveClass("text-destructive")
    unmount()

    v.current = false
    use(withMissing)
    await renderApp("/quality?project=app")
    const stale = await screen.findByRole("article", { name: /Pull request #9/ })
    expect(within(within(stale).getByRole("region", { name: /results table/ })).queryByText("required, no result")).not.toBeInTheDocument()
    expect(within(stale).getByText(/stale/)).toBeInTheDocument()
  })

  test("a pull request with no results and no report says so", async () => {
    await renderApp("/quality?project=app")
    const pr = await screen.findByRole("article", { name: /Pull request #10/ })
    expect(within(pr).getByText("No test results yet.")).toBeInTheDocument()
    expect(within(pr).getByText("no verification report")).toBeInTheDocument()
  })

  test("a cell is a link to its job", async () => {
    const { user, router } = await renderApp("/quality?project=app")
    const pr = await screen.findByRole("article", { name: /Pull request #9/ })
    const cell = within(pr).getByRole("link", { name: /failed, 7 passed, 2 failed/ })
    cell.focus()
    expect(cell).toHaveFocus()
    await user.keyboard("{Enter}")
    await waitFor(() => expect(router.state.location.pathname).toBe("/jobs/JOB-45"))
  })

  test("without a project filter it shows every project, and the filter picks one", async () => {
    const asked: string[] = []
    server.use(
      http.get("/api/v1/quality", ({ request }) => {
        asked.push(new URL(request.url).searchParams.get("project") ?? "")
        return HttpResponse.json(fx.qualityEmpty)
      }),
    )
    const { unmount } = await renderApp("/quality")
    expect(await screen.findByRole("heading", { level: 2, name: "docs" })).toBeInTheDocument()
    expect(screen.getByRole("heading", { level: 2, name: "app" })).toBeInTheDocument()
    await waitFor(() => expect([...asked].sort()).toEqual(["app", "docs"]))
    unmount()

    asked.length = 0
    await renderApp("/quality?project=docs")
    expect(await screen.findByRole("heading", { level: 2, name: "docs" })).toBeInTheDocument()
    expect(screen.queryByRole("heading", { level: 2, name: "app" })).not.toBeInTheDocument()
    expect(asked).toEqual(["docs"])
  })

  test("an unknown project in the address", async () => {
    await renderApp("/quality?project=nope")
    expect(await screen.findByText("Unknown project.")).toBeInTheDocument()
  })

  test("says when the quality of a project cannot be loaded", async () => {
    server.use(http.get("/api/v1/quality", () => HttpResponse.json({ error: { code: "internal", message: "database down" } }, { status: 500 })))
    await renderApp("/quality?project=app")
    expect(await screen.findByText("database down")).toBeInTheDocument()
  })

  test("lists more failures than shown, and several matrix cells of one environment", async () => {
    const cell = fx.quality.changes[0]!.cells[1]!
    use({
      ...fx.qualityEmpty,
      changes: [
        {
          ...fx.quality.changes[1]!,
          cells: [
            { ...cell, job: "JOB-50", cell: { go: "1.25" }, status: "failed", failures_truncated: true, failures: fx.quality.changes[0]!.cells[0]!.failures },
            { ...cell, job: "JOB-51", cell: { go: "1.26" } },
          ],
        },
      ],
    })
    await renderApp("/quality?project=app")
    const pr = await screen.findByRole("article", { name: /Pull request #10/ })
    expect(within(pr).getByRole("link", { name: /go=1.25: failed/ })).toHaveAttribute("href", "/jobs/JOB-50")
    expect(within(pr).getByRole("link", { name: /go=1.26: passed/ })).toHaveAttribute("href", "/jobs/JOB-51")
    expect(within(pr).getByText("More failures than listed.")).toBeInTheDocument()
  })
})
