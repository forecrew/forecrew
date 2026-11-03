import { screen, waitFor } from "@testing-library/react"
import { renderApp } from "@/test/render"

type App = Awaited<ReturnType<typeof renderApp>>

/** Opens the menu with Ctrl+K and waits until the goals, projects and jobs are listed. */
async function openMenu({ user }: App) {
  await screen.findByRole("heading", { name: "Home" })
  await user.keyboard("{Control>}k{/Control}")
  const input = await screen.findByPlaceholderText(/Search pages/)
  await screen.findByRole("option", { name: /GOAL-3/ })
  await screen.findByRole("option", { name: /JOB-41/ })
  await screen.findByRole("option", { name: /docs/ })
  return input
}

/** Types the whole query at once: no per-key delay to race against. */
async function search({ user }: App, input: HTMLElement, query: string) {
  await user.click(input)
  await user.paste(query)
}

describe("command menu", () => {
  test("opens with Ctrl+K and jumps to a goal by ref", async () => {
    const app = await renderApp("/")
    const input = await openMenu(app)
    expect(input).toHaveFocus()
    await search(app, input, "goal-3")
    await app.user.click(await screen.findByRole("option", { name: /GOAL-3/ }))
    await waitFor(() => expect(app.router.state.location.pathname).toBe("/goals/GOAL-3"))
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument()
  })

  test("opens with Cmd+K and toggles closed", async () => {
    const { user } = await renderApp("/")
    await screen.findByRole("heading", { name: "Home" })
    await user.keyboard("{Meta>}k{/Meta}")
    await screen.findByRole("dialog")
    await user.keyboard("{Meta>}k{/Meta}")
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument())
  })

  test("jumps to a job", async () => {
    const app = await renderApp("/")
    await search(app, await openMenu(app), "JOB-41")
    await app.user.click(await screen.findByRole("option", { name: /JOB-41/ }))
    await waitFor(() => expect(app.router.state.location.pathname).toBe("/jobs/JOB-41"))
  })

  test("jumps to a project", async () => {
    const app = await renderApp("/")
    await search(app, await openMenu(app), "acme/docs")
    await app.user.click(await screen.findByRole("option", { name: /docs/ }))
    await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/docs"))
  })

  test("jumps to a page", async () => {
    const app = await renderApp("/")
    await search(app, await openMenu(app), "workers")
    await app.user.click(await screen.findByRole("option", { name: "Workers" }))
    await waitFor(() => expect(app.router.state.location.pathname).toBe("/workers"))
  })

  test("starts New goal", async () => {
    const app = await renderApp("/")
    await openMenu(app)
    await app.user.click(await screen.findByRole("option", { name: "New goal" }))
    await waitFor(() => expect(app.router.state.location.pathname).toBe("/goals/new"))
  })

  test("starts Onboard repository", async () => {
    const app = await renderApp("/")
    await openMenu(app)
    await app.user.click(await screen.findByRole("option", { name: "Onboard repository" }))
    await waitFor(() => expect(app.router.state.location.pathname).toBe("/projects/new"))
  })

  test("says when nothing matches", async () => {
    const app = await renderApp("/")
    await search(app, await openMenu(app), "zzzzzz")
    expect(await screen.findByText("Nothing found.")).toBeInTheDocument()
  })

  test("the command menu button of the narrow header opens it", async () => {
    const { user } = await renderApp("/")
    await user.click(await screen.findByRole("button", { name: "Open command menu" }))
    expect(await screen.findByRole("dialog")).toBeInTheDocument()
  })
})
