import { screen, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const waiting = () => server.use(http.get("/api/v1/config/plans", () => HttpResponse.json(fx.configPlansInbox)))

test("plans waiting for the person show under the chip Setup, next to the decisions", async () => {
  waiting()
  await renderApp("/inbox")
  const setup = await screen.findByRole("region", { name: "Setup" })
  expect(within(setup).getByText("Setup")).toBeInTheDocument()
  const card = within(setup).getByRole("region", { name: "Change plan: Add Windows test machine type" })
  expect(card).toHaveTextContent("Plan from Claude Desktop")
  expect(screen.getByRole("list", { name: "Pending decisions" })).toBeInTheDocument()
})

test("Reject drops a plan", async () => {
  server.use(http.post("/api/v1/config/plans/:plan/cancel", () => HttpResponse.json({ ...fx.configPlan, status: "canceled" })))
  waiting()
  const { user } = await renderApp("/inbox")
  const card = await screen.findByRole("region", { name: /Change plan/ })
  await user.click(within(card).getByRole("button", { name: "Reject" }))
  expect(await within(card).findByText("Canceled")).toBeInTheDocument()
})

test("a plan from an MCP client opens from its link, and plans alone are not an empty inbox", async () => {
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)), http.get("/api/v1/config/plans", () => HttpResponse.json(fx.configPlansEmpty)))
  await renderApp("/inbox?plan=018f0000-0000-7000-8000-000000000730")
  expect(await screen.findByRole("region", { name: "Change plan: Add machine types to Home server" })).toBeInTheDocument()
  expect(screen.queryByText(/Nothing needs you/)).toBeNull()
})

test("an empty inbox says so", async () => {
  server.use(http.get("/api/v1/gates", () => HttpResponse.json(fx.gatesEmpty)), http.get("/api/v1/config/plans", () => HttpResponse.json(fx.configPlansEmpty)))
  await renderApp("/inbox")
  expect(await screen.findByText(/Nothing needs you/)).toBeInTheDocument()
})
