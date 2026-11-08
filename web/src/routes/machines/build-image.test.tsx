import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { renderApp } from "@/test/render"
import { fx, server } from "@/test/server"
import { STAGES, stageState } from "./build-image"

const WIN = "/machines/018f0000-0000-7000-8000-000000000703"
const builds = (data: object) => server.use(http.get("/api/v1/providers/:provider/image-builds", () => HttpResponse.json(data)))
const as = (...perms: string[]) => server.use(http.get("/api/v1/me", () => HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => !perms.includes(p)) })))

async function fillForm(user: Awaited<ReturnType<typeof renderApp>>["user"]) {
  await user.type(await screen.findByLabelText("Windows ISO on this machine"), "D:\\isos\\server-2022.iso")
  await user.type(screen.getByLabelText("ISO checksum (SHA-256)"), "sha256:" + "a1".repeat(32))
  await user.type(screen.getByLabelText("Chocolatey installer hash (SHA-256)"), "b2".repeat(32))
  await user.type(screen.getByLabelText("Image version"), "2026.11.1")
}

test("the build button waits for the fields and the person's own licence statement, then sends references only", async () => {
  let body: unknown
  let registered = false
  server.use(
    http.post("/api/v1/providers/:provider/image-builds", async ({ request, params }) => {
      body = { provider: params.provider, ...((await request.json()) as object) }
      return HttpResponse.json(fx.imageBuildStart, { status: 202 })
    }),
    http.post("/api/v1/images", () => ((registered = true), HttpResponse.json(fx.imageRegister, { status: 201 }))),
  )
  const { user } = await renderApp(WIN)
  expect(await screen.findByRole("heading", { name: "Windows image" })).toBeInTheDocument()
  const button = screen.getByRole("button", { name: "Build the image" })
  expect(button).toBeDisabled()
  await fillForm(user)
  expect(button).toBeDisabled() // the licence is not ticked for them
  await user.click(screen.getByRole("checkbox", { name: "I hold a Windows Server licence" }))
  expect(button).toBeEnabled()
  await user.click(button)
  await waitFor(() => expect(body).toBeDefined())
  expect(body).toEqual({
    provider: "win-host", version: "2026.11.1", iso: "D:\\isos\\server-2022.iso", iso_checksum: "sha256:" + "a1".repeat(32), chocolatey_sha256: "b2".repeat(32), license_confirmed: true,
  })
  expect(registered).toBe(false)
})

test("the edition is sent when given, under More options", async () => {
  let body: Record<string, unknown> = {}
  server.use(http.post("/api/v1/providers/:provider/image-builds", async ({ request }) => ((body = (await request.json()) as typeof body), HttpResponse.json(fx.imageBuildStart, { status: 202 }))))
  const { user } = await renderApp(WIN)
  await fillForm(user)
  await user.click(within(screen.getByRole("form", { name: "Build the Windows image" })).getByRole("button", { name: "More options" }))
  await user.type(screen.getByLabelText("Windows edition"), "Windows Server 2022 SERVERSTANDARD")
  await user.click(screen.getByRole("checkbox", { name: "I hold a Windows Server licence" }))
  await user.click(screen.getByRole("button", { name: "Build the image" }))
  await waitFor(() => expect(body.windows_image_name).toBe("Windows Server 2022 SERVERSTANDARD"))
})

test("a refusal of the server is shown", async () => {
  server.use(http.post("/api/v1/providers/:provider/image-builds", () => HttpResponse.json({ error: { code: "conflict", message: "this location is building an image already: wait for it to end" } }, { status: 409 })))
  const { user } = await renderApp(WIN)
  await fillForm(user)
  await user.click(screen.getByRole("checkbox", { name: "I hold a Windows Server licence" }))
  await user.click(screen.getByRole("button", { name: "Build the image" }))
  expect(await screen.findByRole("alert")).toHaveTextContent("this location is building an image already")
})

test("a build that waits for the connector says so and hides the form", async () => {
  builds(fx.imageBuildsRequested)
  await renderApp(WIN)
  const status = await screen.findByRole("status", { name: "Image build" })
  expect(status).toHaveTextContent("Windows image 2026.11.1")
  expect(status).toHaveTextContent("Waiting for the connector")
  expect(status).toHaveTextContent("within 30 minutes")
  expect(screen.queryByRole("progressbar")).toBeNull()
  expect(screen.queryByRole("button", { name: "Build the image" })).toBeNull()
})

test("a running build shows its percent, its steps and what the machine said as text", async () => {
  builds([{ ...fx.imageBuildsRunning[0], step: "building <b>the</b> image" }])
  await renderApp(WIN)
  const status = await screen.findByRole("status", { name: "Image build" })
  expect(within(status).getByRole("progressbar")).toHaveAttribute("aria-valuenow", "15")
  const steps = within(status).getByRole("list", { name: "Steps of the build" })
  expect(within(steps).getAllByRole("listitem")).toHaveLength(STAGES.length)
  expect(steps).toHaveTextContent("Checking the ISO against its checksum (done)")
  expect(within(steps).getByText(/Building the image \(about an hour\) \(now\)/).closest("li")).toHaveAttribute("aria-current", "step")
  expect(steps).toHaveTextContent("Writing the manifest")
  expect(status).toHaveTextContent("The machine says: building <b>the</b> image")
  expect(status.querySelector("b")).toBeNull()
  expect(screen.queryByRole("button", { name: "Build the image" })).toBeNull()
})

test("a finished build is unsigned: the page shows the way on and registers nothing", async () => {
  builds(fx.imageBuildsDone)
  let registered = false
  server.use(http.post("/api/v1/images", () => ((registered = true), HttpResponse.json(fx.imageRegister, { status: 201 }))))
  const { user } = await renderApp(WIN)
  const built = await screen.findByRole("region", { name: /Windows image 2026.11.1 is built/ })
  expect(built).toHaveTextContent("Not signed, not registered")
  expect(built).toHaveTextContent("forecrew-windows-2026.11.1.manifest.json")
  expect(built).toHaveTextContent("forecrew image sign")
  expect(built).toHaveTextContent("Forecrew never signs or registers it for you")
  expect(screen.getByRole("button", { name: "Build another version" })).toBeInTheDocument()
  await user.click(within(built).getByRole("button", { name: "go to Register the image" }))
  expect(screen.getByRole("textbox", { name: "Windows image" })).toHaveFocus()
  expect(registered).toBe(false)
})

test("a failed build says why, as plain text, and offers a new try", async () => {
  builds(fx.imageBuildsFailed)
  await renderApp(WIN)
  const alert = await screen.findByText("The last build failed")
  const box = alert.closest("[role=alert]")!
  expect(box).toHaveTextContent("get it again from your licence portal <b>now</b>")
  expect(box.querySelector("b")).toBeNull()
  expect(box).toHaveTextContent("Nothing was registered")
  expect(screen.getByRole("button", { name: "Build the image" })).toBeInTheDocument()
  const steps = screen.queryByRole("list", { name: "Steps of the build" })
  expect(steps).toBeNull() // the steps are of a build being followed
})

test("without Manage machines the form is read-only and names the permission", async () => {
  as("worker.manage")
  await renderApp(WIN)
  const button = await screen.findByRole("button", { name: "Build the image" })
  expect(button).toBeDisabled()
  expect(screen.getByText("worker.manage")).toBeInTheDocument()
  expect(screen.getByLabelText("Windows ISO on this machine")).toBeDisabled()
  expect(screen.getByRole("checkbox", { name: "I hold a Windows Server licence" })).toHaveAttribute("aria-disabled", "true")
  // and there is nowhere to register an image either
  expect(screen.queryByRole("form", { name: "Register an image" })).toBeNull()
})

test("an offline connector or a location not yet active cannot start a build", async () => {
  server.use(http.get("/api/v1/providers", () => HttpResponse.json(fx.providers.map((p) => (p.name === "win-host" ? { ...p, online: false } : p)))))
  const { user, unmount } = await renderApp(WIN)
  await fillForm(user)
  await user.click(screen.getByRole("checkbox", { name: "I hold a Windows Server licence" }))
  expect(screen.getByText(/connector on this machine is offline/)).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Build the image" })).toBeDisabled()
  unmount()

  server.use(http.get("/api/v1/providers", () => HttpResponse.json(fx.providers.map((p) => (p.name === "win-host" ? { ...p, status: "pending" } : p)))))
  const again = await renderApp(WIN)
  await fillForm(again.user)
  expect(screen.getByText(/not active yet/)).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Build the image" })).toBeDisabled()
})

test("only a Windows or Proxmox location has the section", async () => {
  await renderApp("/machines/018f0000-0000-7000-8000-000000000700")
  expect(await screen.findByRole("heading", { name: "Home Server" })).toBeInTheDocument()
  expect(screen.queryByRole("heading", { name: "Windows image" })).toBeNull()
})

test("a list that cannot be read is said, and the form stays", async () => {
  server.use(http.get("/api/v1/providers/:provider/image-builds", () => HttpResponse.json({ error: { code: "internal", message: "x" } }, { status: 500 })))
  await renderApp(WIN)
  expect(await screen.findByText("Could not read the builds of this machine.")).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Build the image" })).toBeInTheDocument()
})

test("stages follow the percent the connector reports", () => {
  const at = (state: "requested" | "running" | "done" | "failed", percent: number) => STAGES.map((_, i) => stageState(i, { state, percent }))
  expect(at("requested", 0)).toEqual(["pending", "pending", "pending", "pending", "pending"])
  expect(at("running", 0)).toEqual(["current", "pending", "pending", "pending", "pending"])
  expect(at("running", 15)).toEqual(["done", "done", "done", "current", "pending"])
  expect(at("running", 95)).toEqual(["done", "done", "done", "done", "current"])
  expect(at("failed", 5)).toEqual(["done", "failed", "pending", "pending", "pending"])
  expect(at("done", 100)).toEqual(["done", "done", "done", "done", "done"])
})

test("someone who cannot manage machines reads that registering the image needs the permission", async () => {
  as("worker.manage")
  builds(fx.imageBuildsDone)
  await renderApp(WIN)
  const built = await screen.findByRole("region", { name: /Windows image 2026.11.1 is built/ })
  expect(built).toHaveTextContent("which needs worker.manage")
  expect(within(built).queryByRole("button", { name: "go to Register the image" })).toBeNull()
})

test("an instance-wide location is offered to an instance admin only, and others do not even ask for its builds", async () => {
  const hosted = fx.providers.map((p) => (p.name === "win-host" ? { ...p, ownership: "hosted" } : p))
  let asked = 0
  server.use(
    http.get("/api/v1/providers", () => HttpResponse.json(hosted)),
    http.get("/api/v1/providers/:provider/image-builds", () => ((asked += 1), HttpResponse.json({ error: { code: "not_found", message: "nope" } }, { status: 404 }))),
  )
  const first = await renderApp(WIN)
  expect(await screen.findByText(/belongs to the whole instance/)).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "Build the image" })).toBeDisabled()
  expect(screen.getByText("Needs an instance admin")).toBeInTheDocument()
  expect(screen.queryByLabelText("Windows ISO on this machine")).toBeNull()
  expect(asked).toBe(0)
  first.unmount()

  server.use(
    http.get("/api/v1/me", () => HttpResponse.json({ ...fx.me, user: { id: "018f0000-0000-7000-8000-000000000300", name: "ana", instance_admin: true } })),
    http.get("/api/v1/providers/:provider/image-builds", () => HttpResponse.json([])),
  )
  await renderApp(WIN)
  expect(await screen.findByLabelText("Windows ISO on this machine")).toBeEnabled()
  expect(screen.queryByText("Needs an instance admin")).toBeNull()
})
