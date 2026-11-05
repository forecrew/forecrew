import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

test("lists the decisions of every project with who decided and what they rest on", async () => {
  await renderApp("/decisions")
  const list = await screen.findByRole("list", { name: "Decisions" })
  const [first, second] = within(list).getAllByRole("listitem")
  expect(within(first!).getByText("DEC-5")).toBeInTheDocument()
  expect(within(first!).getByText("Which queue should jobs use?")).toBeInTheDocument()
  expect(within(first!).getByText("postgres-queue")).toBeInTheDocument()
  expect(within(first!).getByText("by user:ana")).toBeInTheDocument()
  expect(within(first!).getByText(/among postgres-queue, redis-streams/)).toBeInTheDocument()
  expect(within(first!).getByText("Already run; no new service.")).toBeInTheDocument()
  expect(within(first!).getByText(/job data should not share the application database/)).toBeInTheDocument()
  expect(within(first!).getByText("A-7")).toBeInTheDocument()
  expect(within(first!).getByText("app")).toBeInTheDocument()
  expect(within(second!).getByText("by policy")).toBeInTheDocument()
  expect(within(second!).queryByText(/Dissent/)).not.toBeInTheDocument()
})

test("links a decision to its council", async () => {
  const { user, router } = await renderApp("/decisions?project=app")
  const list = await screen.findByRole("list", { name: "Decisions" })
  await user.click(within(within(list).getAllByRole("listitem")[0]!).getByRole("link", { name: "Open the council" }))
  await waitFor(() => expect(router.state.location.pathname).toBe(`/councils/${fx.decisions[0]!.council_id}`))
})

test("asks only for the project of the address", async () => {
  const seen: string[] = []
  server.use(
    http.get("/api/v1/decisions", ({ request }) => {
      seen.push(new URL(request.url).search)
      return HttpResponse.json(fx.decisions)
    }),
  )
  await renderApp("/decisions?project=app")
  await screen.findByRole("list", { name: "Decisions" })
  expect(seen).toEqual(["?project=app"])
  expect(screen.queryByText("in app")).not.toBeInTheDocument()
})

test("a decision without a council, options or assumptions", async () => {
  const bare = { ...fx.decisions[1]!, council_id: undefined, options: null, assumptions: [], reasons: "" }
  server.use(http.get("/api/v1/decisions", () => HttpResponse.json([bare])))
  await renderApp("/decisions?project=app")
  expect(await screen.findByText("Not decided by a council.")).toBeInTheDocument()
  expect(screen.queryByText(/among/)).not.toBeInTheDocument()
  expect(screen.queryByText(/Rests on/)).not.toBeInTheDocument()
})

test("says when nothing was decided", async () => {
  server.use(http.get("/api/v1/decisions", () => HttpResponse.json(fx.decisionsEmpty)))
  await renderApp("/decisions")
  expect(await screen.findByText(/No decisions yet/)).toBeInTheDocument()
})
