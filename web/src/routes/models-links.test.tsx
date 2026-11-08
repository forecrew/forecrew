import { screen, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import type { ModelCheck, ModelConnectionSpec } from "@/api/types"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"
import { connectionTitle, keyAdvice, keyStatus, levelText, localEndpoint, whereText } from "@/lib/models"

/** No model connection yet: the first run links to AI models. */
const noKey = () =>
  server.use(
    http.get("/api/v1/config/resources", ({ request }) =>
      HttpResponse.json(new URL(request.url).searchParams.get("kind") === "model_connection" ? [] : fx.configResourcesProvider),
    ),
  )

test("a job waiting for a model connection links to its fix", async () => {
  server.use(http.get("/api/v1/jobs/:ref", () => HttpResponse.json(fx.jobWaitingPrivateCode)))
  await renderApp("/jobs/JOB-43")
  const banner = await screen.findByRole("status", { name: "Why it waits" })
  expect(banner).toHaveTextContent("connection deepseek is not authorized for project app")
  expect(within(banner).getByRole("link", { name: "Review AI models" })).toHaveAttribute("href", "/settings/models")
})

test("someone who may not fix a model connection asks an admin", async () => {
  server.use(
    http.get("/api/v1/jobs/:ref", () => HttpResponse.json(fx.jobWaitingPrivateCode)),
    http.get("/api/v1/me", () => HttpResponse.json({ ...fx.me, permissions: fx.me.permissions.filter((p) => p !== "models.assign") })),
  )
  await renderApp("/jobs/JOB-43")
  expect(await screen.findByRole("button", { name: "Ask an admin" })).toBeInTheDocument()
})

test("a computer that could not install an agent says so on the machine", async () => {
  await renderApp("/machines")
  const table = await screen.findByRole("region", { name: "Locations table" })
  expect(within(table).getByText(/Codex was downloaded on Build Box 1 but did not match its checksum/)).toBeInTheDocument()
})

test("first-run setup and Home link to AI models while no key is set", async () => {
  localStorage.clear()
  noKey()
  const { unmount } = await renderApp("/setup")
  const first = await screen.findByRole("region", { name: "No AI provider yet" })
  expect(within(first).getByRole("link", { name: "Add a key" })).toHaveAttribute("href", "/settings/models")
  unmount()
  await renderApp("/")
  const finish = await screen.findByRole("region", { name: "Finish setting up" })
  expect(await within(finish).findByRole("link", { name: "Add an AI provider key" })).toHaveAttribute("href", "/settings/models")
})

test("setup says nothing of keys once one is set", async () => {
  await renderApp("/setup")
  await screen.findByRole("heading", { name: "Set up Forecrew" })
  expect(screen.queryByRole("region", { name: "No AI provider yet" })).toBeNull()
})

describe("model words", () => {
  const conn = (spec: Partial<ModelConnectionSpec>, name = "x", local = false) => ({ name, spec: { vendor: "x", adapter: "opencode", enabled: true, ...spec }, fromFile: false, local })
  const check = (status: string): ModelCheck => ({ id: "1", connection: "x", job_id: "j", status, created_at: "", updated_at: "" })

  test("local addresses, as the server reads them", () => {
    for (const url of ["http://localhost:1234", "http://LOCALHOST", "http://127.0.0.1", "http://10.0.0.2", "http://172.20.0.1", "http://192.168.1.1", "http://[::1]:8080", "http://[fd00::1]/"]) expect(localEndpoint(url)).toBe(true)
    // The server takes only localhost itself, not a name under it.
    for (const url of [undefined, "", "not a url", "https://api.deepseek.com", "http://172.32.0.1", "http://8.8.8.8", "http://x.localhost", "http://a.localhost:11434"]) expect(localEndpoint(url)).toBe(false)
  })

  test("every key check status has words", () => {
    for (const s of ["refused", "unreachable", "wrong_address", "unsupported", "failed", "queued"]) expect(keyStatus(check(s)).text).not.toBe("")
    expect(keyStatus(undefined).text).toBe("Not checked yet")
    expect(keyAdvice(check("refused"))).toMatch(/Enter the key again/)
    expect(keyAdvice(check("wrong_address"))).toMatch(/More options/)
    expect(keyAdvice(check("reachable"))).toBe("")
  })

  test("test results in words", () => {
    expect(levelText(check("queued")).text).toBe("Testing…")
    expect(levelText(check("failed")).text).toBe("The test did not run")
    expect(levelText({ ...check("done"), test: { abilities: { read: true, edit: true, command: true, mcp: true, json: true }, level: "none", tokens: 0, duration_ms: 0, turns: 0, cost_usd: 0 } }).text).toBe("Did not pass")
  })

  test("where a provider runs and how a connection is called", () => {
    const region = { name: "bedrock", title: "Amazon Bedrock", adapter: "opencode", key_env: "K", homepage: "", country: "region" }
    expect(whereText(conn({}), region)).toBe("In the region of its endpoint")
    expect(whereText(conn({}))).toBe("Another endpoint")
    expect(connectionTitle(conn({}, "my-models"))).toBe("My Models")
    // Only the server's flag makes a connection local: not the provider's country, not the address.
    const ollama = { name: "ollama", title: "Ollama", adapter: "opencode", key_env: "K", homepage: "", country: "local" }
    expect(whereText(conn({ vendor: "ollama", base_url: "https://ollama.example.com/v1" }), ollama)).toBe("Another endpoint")
    expect(whereText(conn({ vendor: "ollama", base_url: "http://x.localhost:11434/v1" }), ollama)).toBe("Another endpoint")
    expect(whereText(conn({ vendor: "ollama" }, "home", true), ollama)).toBe("Local, code stays here")
  })
})
