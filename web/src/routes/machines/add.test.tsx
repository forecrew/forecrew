import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"
import { commandFor, proposeTypes } from "./join"

const CLAIMED = fx.joinCodes[0]!
const created = () => http.post("/api/v1/join-codes", () => HttpResponse.json({ ...fx.joinCodeCreate, join_code: { ...fx.joinCodeCreate.join_code, id: CLAIMED.id } }, { status: 201 }))

test("asks what you want to connect, and points to GitHub Actions for tests only", async () => {
  await renderApp("/machines/add")
  expect(await screen.findByRole("heading", { name: "Add machines" })).toBeInTheDocument()
  expect(screen.getByRole("link", { name: /A computer or server/ })).toBeInTheDocument()
  expect(screen.getByText(/GitHub Actions already runs them/)).toBeInTheDocument()
  expect(screen.getByText(/Tell the assistant what you have/i)).toBeInTheDocument()
})

test("offers Forecrew Cloud only where the server has machine types of its own", async () => {
  server.use(http.get("/api/v1/shapes", () => HttpResponse.json([{ ...fx.shapes[0], ownership: "hosted" }])))
  await renderApp("/machines/add")
  expect(await screen.findByRole("link", { name: /Forecrew Cloud/ })).toBeInTheDocument()
})

test("someone who cannot manage machines is sent to an admin", async () => {
  server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => p !== "worker.manage") })))
  await renderApp("/machines/add?kind=computer")
  expect(await screen.findByText(/needs Manage machines/)).toBeInTheDocument()
  expect(screen.queryByRole("button", { name: "Get the command" })).toBeNull()
})

test("a computer joins: command, checks as plain text, confirm, a plan of machine types, a test job", async () => {
  server.use(created())
  let confirmed: unknown
  let planned: { changes: { kind: string; name: string; spec: Record<string, unknown> }[] } | undefined
  let dispatched: unknown
  server.use(
    http.post("/api/v1/join-codes/:join/confirm", async ({ request }) => ((confirmed = await request.json()), HttpResponse.json(fx.joinConfirm))),
    http.post("/api/v1/config/plans", async ({ request }) => ((planned = (await request.json()) as typeof planned), HttpResponse.json(fx.configPlan, { status: 201 }))),
    http.post("/api/v1/jobs", async ({ request }) => ((dispatched = await request.json()), HttpResponse.json(fx.jobDispatch, { status: 201 }))),
  )
  const { user } = await renderApp("/machines/add?kind=computer")
  await user.click(await screen.findByRole("button", { name: "Get the command" }))
  expect(await screen.findByLabelText("Join code")).toHaveTextContent("K7PQ-2M4X")
  // the code is never in the command line: it is typed when asked
  expect(screen.getByLabelText("Command")).not.toHaveTextContent("K7PQ")
  expect(screen.getByLabelText("Command")).toHaveTextContent("install.sh")
  await user.click(screen.getByRole("tab", { name: "Windows" }))
  expect(screen.getByLabelText("Command")).toHaveTextContent("Read-Host 'Join code'")
  const checks = await screen.findByText("Connected from home-nas")
  const list = checks.closest("ul")!
  expect(list).toHaveTextContent("Linux, 16 CPU, 64 GB")
  expect(list).toHaveTextContent("disk: 412 GB free")
  // what the machine said is text, never markup
  expect(list).toHaveTextContent("sleep: This <b>Mac</b> goes to sleep (check this)")
  const name = screen.getByLabelText("Name")
  expect(name).toHaveValue("home-nas")
  await user.click(screen.getByRole("button", { name: "Confirm" }))
  expect(confirmed).toEqual({ name: "home-nas", facts_hash: "f3a9c1" })
  await user.click(await screen.findByRole("button", { name: "Suggest machine types" }))
  await screen.findByRole("region", { name: /Change plan/ })
  expect(planned?.changes.map((c) => c.name)).toEqual(["home-server-small", "home-server-medium", "home-server-large", "home-server"])
  expect(planned?.changes[3]?.spec).toMatchObject({ kind: "microvm", limits: { max_machines: 3 } })
  await user.click(screen.getByRole("button", { name: "Confirm" })) // the plan
  await screen.findByText("Applied")
  await user.click(await screen.findByRole("button", { name: "Run a test job" }))
  await waitFor(() => expect(dispatched).toMatchObject({ project: "app", role: "reviewer" }))
  expect(await screen.findByText(/Started a test job in app/)).toBeInTheDocument()
})

test("a Windows host with Hyper-V gets the image step and can register the signed image", async () => {
  const hyperv = { ...CLAIMED, kind: "hyperv", facts: { ...CLAIMED.facts, os: ["windows"] } }
  server.use(created(), http.get("/api/v1/join-codes", () => HttpResponse.json([hyperv])))
  let body: unknown
  server.use(http.post("/api/v1/images", async ({ request }) => ((body = await request.json()), HttpResponse.json(fx.imageRegister, { status: 201 }))))
  const { user } = await renderApp("/machines/add?kind=computer")
  await user.click(await screen.findByRole("button", { name: "Get the command" }))
  const image = await screen.findByRole("heading", { name: "Windows image" })
  expect(image.closest("section")).toHaveTextContent("Forecrew cannot hand out Windows")
  const form = screen.getByRole("form", { name: "Register an image" })
  await user.click(within(form).getByLabelText("Windows image"))
  await user.paste("not json")
  await user.click(within(form).getByRole("button", { name: "Register the image" }))
  expect(await within(form).findByText(/not JSON/)).toBeInTheDocument()
  await user.clear(within(form).getByLabelText("Windows image"))
  await user.click(within(form).getByLabelText("Windows image"))
  await user.paste('{"name":"windows-11","version":"1"}')
  await user.click(within(form).getByRole("button", { name: "Register the image" }))
  expect(await within(form).findByText("Registered windows-11 1.")).toBeInTheDocument()
  expect(body).toEqual({ name: "windows-11", version: "1" })
})

test("a Hyper-V host can build its Windows image once the location is added, and not before", async () => {
  const win = fx.providers.find((p) => p.name === "win-host")!
  const hyperv = { ...CLAIMED, kind: "hyperv", facts: { ...CLAIMED.facts, os: ["windows"] } }
  server.use(
    created(),
    http.get("/api/v1/join-codes", () => HttpResponse.json([hyperv])),
    http.post("/api/v1/join-codes/:join/confirm", () => HttpResponse.json({ ...win, status: "pending" })),
    http.post("/api/v1/config/plans", () => HttpResponse.json(fx.configPlan, { status: 201 })),
  )
  const { user } = await renderApp("/machines/add?kind=computer")
  await user.click(await screen.findByRole("button", { name: "Get the command" }))
  await user.click(await screen.findByRole("button", { name: "Confirm" }))
  await user.click(await screen.findByRole("button", { name: "Suggest machine types" }))
  await screen.findByRole("region", { name: /Change plan/ })
  expect(screen.queryByRole("form", { name: "Build the Windows image" })).toBeNull() // not before the plan is applied
  await user.click(screen.getByRole("button", { name: "Confirm" }))
  await screen.findByText("Applied")
  const form = await screen.findByRole("form", { name: "Build the Windows image" })
  expect(within(form).getByRole("button", { name: "Build the image" })).toBeDisabled()
  expect(screen.getByText(/Once this location is added, you can build the image/)).toBeInTheDocument()
})

test("a failed confirmation says why", async () => {
  server.use(created(), http.post("/api/v1/join-codes/:join/confirm", () => HttpResponse.json({ error: { code: "conflict", message: "the report changed" } }, { status: 409 })))
  const { user } = await renderApp("/machines/add?kind=computer")
  await user.click(await screen.findByRole("button", { name: "Get the command" }))
  await user.click(await screen.findByRole("button", { name: "Confirm" }))
  expect(await screen.findByText("the report changed")).toBeInTheDocument()
})

test("a customer AWS account needs a monthly limit and gets a seven day code for the cloud team", async () => {
  const { user } = await renderApp("/machines/add?kind=aws")
  expect(await screen.findByText("Enter a monthly limit to continue.")).toBeInTheDocument()
  await user.type(screen.getByLabelText("Monthly limit"), "4100")
  await user.click(screen.getByRole("button", { name: "Get the join code" }))
  expect(await screen.findByText(/lasts 7 days/)).toBeInTheDocument()
  expect(screen.queryByLabelText("Command")).toBeNull()
})

test("sizes follow the hardware, and the limit uses at most 75% of the host", () => {
  expect(proposeTypes({ max_cpu: 16, max_memory_mb: 65536 })).toEqual({ sizes: [{ label: "small", cpu: 2, memory_mb: 4096 }, { label: "medium", cpu: 4, memory_mb: 8192 }, { label: "large", cpu: 8, memory_mb: 16384 }], limit: 3 })
  expect(proposeTypes({ max_cpu: 4, max_memory_mb: 8192, max_machines: 2 })).toEqual({ sizes: [{ label: "small", cpu: 2, memory_mb: 4096 }, { label: "medium", cpu: 4, memory_mb: 8192 }], limit: 1 })
  expect(proposeTypes({ max_cpu: 1, max_memory_mb: 1024 }).sizes).toHaveLength(1)
  expect(commandFor("macos", "https://x.test")).toContain("sh install.sh --server https://x.test")
})
