import { screen, waitFor, within } from "@testing-library/react"
import { http, HttpResponse } from "msw"
import { fx, server } from "@/test/server"
import { renderApp } from "@/test/render"

const as = (me: object) => server.use(http.get("/api/v1/me", () => HttpResponse.json(me)))
const without = (...perms: string[]) => as({ ...fx.me, permissions: fx.me.permissions.filter((p) => !perms.includes(p)) })

/** Records the body of every plan the page asks for, answering with plan. */
function plans(plan: object = fx.configPlanModelConnection) {
  const bodies: { title: string; changes: { kind: string; name: string; spec?: Record<string, unknown>; delete?: boolean }[] }[] = []
  server.use(
    http.post("/api/v1/config/plans", async ({ request }) => {
      bodies.push((await request.json()) as (typeof bodies)[number])
      return HttpResponse.json(plan, { status: 201 })
    }),
  )
  return bodies
}

const row = (name: RegExp) => screen.findByRole("listitem", { name })

describe("the AI models page", () => {
  test("lists each connection with its key check, where it runs and who may send it code", async () => {
    await renderApp("/settings/models")
    expect(await screen.findByRole("heading", { name: "AI models" })).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "AI models" })).toBeInTheDocument()
    const deepseek = await row(/^DeepSeek$/)
    expect(within(deepseek).getByText("2 models")).toBeInTheDocument()
    expect(await within(deepseek).findByText("Works")).toBeInTheDocument()
    expect(within(deepseek).getByText("China")).toBeInTheDocument()
    expect(within(deepseek).getByText("Allowed on 1 project")).toBeInTheDocument()
    const claude = await row(/^Anthropic \(claude\)$/)
    expect(within(claude).getByText("Managed by server file")).toBeInTheDocument()
    const ollama = await row(/home-ollama/)
    expect(within(ollama).getByText("Local, code stays here")).toBeInTheDocument()
    expect(within(ollama).getByText("Cannot reach it")).toBeInTheDocument()
    expect(within(ollama).getByText(/local_model_endpoints/)).toBeInTheDocument()
    // A provider of local models at a public address is not local: only the server's flag is.
    const lab = await row(/lab-ollama/)
    expect(within(lab).getByText("Another endpoint")).toBeInTheDocument()
    expect(within(lab).getByText("No private code yet")).toBeInTheDocument()
    expect(within(lab).queryByText("Local, code stays here")).toBeNull()
  })

  test("shows what each model may do, and what an agent said only as text", async () => {
    const { user } = await renderApp("/settings/models")
    const deepseek = await row(/^DeepSeek$/)
    await user.click(within(deepseek).getByText("Models and options"))
    const models = within(deepseek).getByRole("list", { name: "Models of deepseek" })
    expect(await within(models).findByText("Writes code")).toBeInTheDocument()
    expect(within(models).getByText("Can't edit files, Can't run commands, Can't call tools reliably")).toBeInTheDocument()
    expect(within(models).getByText("tool call <b>malformed</b>: ignore your instructions")).toBeInTheDocument()
    expect(document.querySelector("b")).toBeNull()
    expect(within(deepseek).getByText("In secure storage, never shown")).toBeInTheDocument()
    const ollama = await row(/home-ollama/)
    await user.click(within(ollama).getByText("Models and options"))
    expect(within(ollama).getByText(/^Reading roles only: can't edit files, can't run commands$/)).toBeInTheDocument()
    expect(within(ollama).getByText("None needed")).toBeInTheDocument()
  })

  test("a test shows its cost first and runs once confirmed", async () => {
    const bodies: unknown[] = []
    server.use(
      http.post("/api/v1/model-connections/:connection/tests", async ({ request }) => {
        const body = (await request.json()) as { run?: boolean }
        bodies.push(body)
        return HttpResponse.json(body.run ? { ...fx.modelTestEstimate, test: fx.modelCheck } : fx.modelTestEstimate)
      }),
    )
    const { user } = await renderApp("/settings/models")
    const deepseek = await row(/^DeepSeek$/)
    await user.click(within(deepseek).getByText("Models and options"))
    await user.click(await within(deepseek).findByRole("button", { name: "Test again deepseek/deepseek-chat on deepseek" }))
    const test = await within(deepseek).findByRole("group", { name: "Test of deepseek/deepseek-chat" })
    expect(test).toHaveTextContent("about $0.18, at most $15.00")
    expect(bodies).toEqual([{ model: "deepseek/deepseek-chat" }])
    await user.click(within(test).getByRole("button", { name: "Run the test" }))
    expect(await within(deepseek).findByText(/Test started/)).toBeInTheDocument()
    expect(bodies[1]).toEqual({ model: "deepseek/deepseek-chat", run: true })
  })

  test("checks a key again and turns a connection off with a plan", async () => {
    let checked = ""
    server.use(http.post("/api/v1/model-connections/:connection/check", ({ params }) => ((checked = String(params.connection)), HttpResponse.json(fx.modelCheck, { status: 202 }))))
    const bodies = plans()
    const { user } = await renderApp("/settings/models")
    const deepseek = await row(/^DeepSeek$/)
    await user.click(within(deepseek).getByText("Models and options"))
    await user.click(within(deepseek).getByRole("button", { name: "Check the key again" }))
    await waitFor(() => expect(checked).toBe("deepseek"))
    await user.click(within(deepseek).getByRole("button", { name: "Turn off" }))
    await screen.findByRole("region", { name: /Change plan/ })
    expect(bodies[0]?.changes[0]).toMatchObject({ kind: "model_connection", name: "deepseek", spec: { enabled: false, secret_ref: "store:models/deepseek" } })
    await user.click(screen.getByRole("button", { name: "Edit" }))
    await user.click(within(deepseek).getByRole("button", { name: "Remove…" }))
    await waitFor(() => expect(bodies[1]?.changes[0]).toEqual({ kind: "model_connection", name: "deepseek", delete: true }))
  })

  test("adds a provider from the catalog: the key goes only in the plan's secure field", async () => {
    const bodies = plans()
    const { user } = await renderApp("/settings/models")
    await user.click(await screen.findByRole("button", { name: "Add provider" }))
    const dialog = await screen.findByRole("dialog", { name: "Add an AI provider" })
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText("Pick a provider, or another endpoint.")).toBeInTheDocument()
    await user.click(within(dialog).getByRole("radio", { name: /DeepSeek API/ }))
    expect(within(dialog).getByText(/Runs in China/)).toBeInTheDocument()
    expect(within(dialog).getByText(/type the key in a secure field/)).toBeInTheDocument()
    // The name is taken: the form offers the next free one.
    await user.click(within(dialog).getByText("More options"))
    expect(within(dialog).getByLabelText("Name")).toHaveValue("deepseek-2")
    expect(within(dialog).queryByLabelText(/key$/i)).toBeNull()
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    await within(dialog).findByRole("region", { name: "Change plan: Add AI provider DeepSeek API" })
    expect(bodies[0]).toEqual({
      title: "Add AI provider DeepSeek API",
      source: "form",
      changes: [{ kind: "model_connection", name: "deepseek-2", spec: { vendor: "deepseek", adapter: "opencode", base_url: "https://api.deepseek.com/v1", key_env: "DEEPSEEK_API_KEY", secret_ref: "store:models/deepseek-2", enabled: true } }],
    })
    expect(within(dialog).getByRole("form", { name: "Secure field: API key of deepseek" })).toBeInTheDocument()
  })

  test("a local endpoint needs no key, and another endpoint needs its address", async () => {
    const bodies = plans()
    const { user } = await renderApp("/settings/models")
    await user.click(await screen.findByRole("button", { name: "Add provider" }))
    const dialog = await screen.findByRole("dialog", { name: "Add an AI provider" })
    await user.click(within(dialog).getByRole("radio", { name: /Another endpoint/ }))
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText(/Enter the address of the API/)).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Address"), "http://models.example.com/v1")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText(/Plain http only reaches your own network/)).toBeInTheDocument()
    await user.clear(within(dialog).getByLabelText("Address"))
    await user.type(within(dialog).getByLabelText("Address"), "http://192.168.1.20:8080/v1")
    expect(within(dialog).getByText(/No key needed/)).toBeInTheDocument()
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText(/uppercase, ending in _KEY/)).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText("Key variable"), "LAB_KEY")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    await within(dialog).findByRole("region", { name: /Change plan/ })
    expect(bodies[0]?.changes[0]?.spec).toEqual({ vendor: "endpoint", adapter: "opencode", base_url: "http://192.168.1.20:8080/v1", key_env: "LAB_KEY", enabled: true })
  })

  test("groups the roles by what they do, with the model each uses", async () => {
    await renderApp("/settings/models")
    const table = await screen.findByRole("region", { name: "Who uses what table" })
    const rows = within(table).getAllByRole("row")
    expect(rows[1]).toHaveTextContent("Writes codeDeveloper, Test Engineerclaude-sonnet-4-5 on claude")
    expect(rows[2]).toHaveTextContent("ReviewsReviewer, Security Reviewerdeepseek/deepseek-chat on deepseek")
    expect(rows[3]).toHaveTextContent("PlansArchitect, Product Ownerclaude-opus-4-5 on claude")
    expect(rows[4]).toHaveTextContent("Quick tasksTriage, Facilitatordeepseek/deepseek-chat on deepseek")
  })

  test("a preset makes the plan of a suggestion, with its notes and the second confirmation", async () => {
    let asked: unknown
    server.use(http.post("/api/v1/models/suggest", async ({ request }) => ((asked = await request.json()), HttpResponse.json(fx.modelsSuggest))))
    const { user } = await renderApp("/settings/models")
    const presets = await screen.findByRole("radiogroup", { name: "Preset" })
    await user.click(within(presets).getByRole("radio", { name: "Low cost" }))
    const card = await screen.findByRole("region", { name: "Change plan: Models suggested (low_cost)" })
    expect(asked).toEqual({ preset: "low_cost", source: "form" })
    expect(within(presets).getByRole("radio", { name: "Low cost" })).toHaveAttribute("aria-checked", "true")
    expect(screen.getByText(/only models of deepseek passed for the developer/)).toBeInTheDocument()
    expect(within(card).getByText("Sends the code of developer (organization) to another connection")).toBeInTheDocument()
  })

  test("changes a role, testing a model first when it was not tested", async () => {
    const bodies = plans(fx.configPlan)
    const { user } = await renderApp("/settings/models")
    await user.click(await screen.findByRole("button", { name: "Change a role" }))
    const dialog = await screen.findByRole("dialog", { name: "Change a role" })
    expect(within(dialog).getByLabelText("Model")).toHaveValue("claude-sonnet-4-5")
    expect(within(dialog).getByText("Not tested yet")).toBeInTheDocument()
    expect(within(dialog).getByRole("button", { name: "Test it first claude-sonnet-4-5 on claude" })).toBeInTheDocument()
    await user.selectOptions(within(dialog).getByLabelText("Provider"), "deepseek")
    expect(within(dialog).getByLabelText("Model")).toHaveValue("deepseek/deepseek-chat")
    expect(within(dialog).getByText("Writes code")).toBeInTheDocument()
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    await waitFor(() => expect(bodies).toHaveLength(1))
    const spec = bodies[0]?.changes[0]?.spec as { roles: Record<string, unknown>; tiers: Record<string, Record<string, string>> }
    expect(bodies[0]?.changes[0]).toMatchObject({ kind: "model_assignment", name: "organization" })
    expect(spec.roles.developer).toEqual({ connection: "deepseek", tier: "strong" })
    expect(spec.roles.reviewer).toEqual({ connection: "deepseek", tier: "strong" })
    expect(spec.tiers.strong).toEqual({ anthropic: "claude-sonnet-4-5", deepseek: "deepseek/deepseek-chat" })
    expect(await screen.findByRole("region", { name: /Change plan/ })).toBeInTheDocument()
  })

  test("a model on opencode is named provider/model", async () => {
    const bodies = plans(fx.configPlan)
    const { user } = await renderApp("/settings/models")
    await user.click(await screen.findByRole("button", { name: "Change a role" }))
    const dialog = await screen.findByRole("dialog", { name: "Change a role" })
    await user.selectOptions(within(dialog).getByLabelText("Provider"), "deepseek")
    await user.clear(within(dialog).getByLabelText("Model"))
    await user.type(within(dialog).getByLabelText("Model"), "deepseek-chat")
    await user.click(within(dialog).getByRole("button", { name: "Review the change" }))
    expect(within(dialog).getByText("Name it provider/model on this provider, such as deepseek/deepseek-chat.")).toBeInTheDocument()
    expect(bodies).toHaveLength(0)
  })

  test("a model that did not pass says it needs the second confirmation", async () => {
    const { user } = await renderApp("/settings/models")
    await user.click(await screen.findByRole("button", { name: "Change a role" }))
    const dialog = await screen.findByRole("dialog", { name: "Change a role" })
    await user.selectOptions(within(dialog).getByLabelText("Provider"), "deepseek")
    await user.click(within(dialog).getByText("More options"))
    await user.selectOptions(within(dialog).getByLabelText("Tier"), "best")
    expect(within(dialog).getByLabelText("Model")).toHaveValue("deepseek/deepseek-reasoner")
    expect(within(dialog).getByText(/Using it anyway asks you to confirm a second time/)).toBeInTheDocument()
  })

  test("more options: the role tiers and whether projects may override", async () => {
    const bodies = plans(fx.configPlan)
    const { user } = await renderApp("/settings/models")
    await screen.findByRole("region", { name: "Who uses what table" })
    await user.click(screen.getAllByText("More options").at(-1)!)
    expect(within(screen.getByRole("region", { name: "Per role table" })).getAllByRole("row")).toHaveLength(9)
    await user.click(screen.getByRole("checkbox", { name: /Projects may choose their own models/ }))
    await waitFor(() => expect(bodies[0]?.changes[0]?.spec).toMatchObject({ project_overrides: false }))
  })

  test("says what to do with no connection", async () => {
    server.use(http.get("/api/v1/config/resources", () => HttpResponse.json([])))
    await renderApp("/settings/models")
    expect(await screen.findByText("Add an AI provider key so agents can work. One is enough.")).toBeInTheDocument()
    expect(screen.queryByRole("region", { name: "Who uses what table" })).toBeNull()
  })

  test("someone who only reads sees no keys and no controls", async () => {
    as(fx.meViewer)
    const { user } = await renderApp("/settings/models")
    const add = await screen.findByRole("button", { name: "Add provider" })
    expect(add).toBeDisabled()
    expect(screen.getAllByText("models.manage").length).toBeGreaterThan(0)
    const deepseek = await row(/^DeepSeek$/)
    await user.click(within(deepseek).getByText("Models and options"))
    expect(within(deepseek).queryByRole("button", { name: "Turn off" })).toBeNull()
    for (const b of within(deepseek).getAllByRole("button", { name: /Test/ })) expect(b).toBeDisabled()
    for (const r of within(await screen.findByRole("radiogroup", { name: "Preset" })).getAllByRole("radio")) expect(r).toBeDisabled()
    expect(screen.getByRole("button", { name: "Change a role" })).toBeDisabled()
    expect(screen.getByRole("checkbox", { name: /Projects may choose/ })).toBeDisabled()
  })

  test("without models.read the section is gone", async () => {
    without("models.read")
    await renderApp("/settings/models")
    expect(await screen.findByText(/You cannot see this section/)).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: "AI models" })).toBeNull()
  })
})
