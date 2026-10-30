import { screen, waitFor } from "@testing-library/react"
import { renderApp } from "@/test/render"

describe("command menu", () => {
  test("opens with Ctrl+K and jumps to a goal by ref", async () => {
    const { user, router } = await renderApp("/")
    await screen.findByRole("heading", { name: "Home" })
    await user.keyboard("{Control>}k{/Control}")
    const input = await screen.findByPlaceholderText(/Search pages/)
    expect(input).toHaveFocus()
    await user.type(input, "goal-3")
    await user.click(await screen.findByRole("option", { name: /GOAL-3/ }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/goals/GOAL-3"))
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

  test("jumps to a job, a project and a page", async () => {
    const { user, router } = await renderApp("/")
    await screen.findByRole("heading", { name: "Home" })
    await user.keyboard("{Control>}k{/Control}")
    await user.type(await screen.findByPlaceholderText(/Search pages/), "JOB-41")
    await user.click(await screen.findByRole("option", { name: /JOB-41/ }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/jobs/JOB-41"))

    await user.keyboard("{Control>}k{/Control}")
    await user.type(await screen.findByPlaceholderText(/Search pages/), "acme/docs")
    await user.click(await screen.findByRole("option", { name: /docs/ }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/projects/docs"))

    await user.keyboard("{Control>}k{/Control}")
    await user.type(await screen.findByPlaceholderText(/Search pages/), "workers")
    await user.click(await screen.findByRole("option", { name: "Workers" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/workers"))
  })

  test("starts New goal and Onboard repository", async () => {
    const { user, router } = await renderApp("/")
    await screen.findByRole("heading", { name: "Home" })
    await user.keyboard("{Control>}k{/Control}")
    await user.click(await screen.findByRole("option", { name: "New goal" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/goals/new"))
    await user.keyboard("{Control>}k{/Control}")
    await user.click(await screen.findByRole("option", { name: "Onboard repository" }))
    await waitFor(() => expect(router.state.location.pathname).toBe("/projects/new"))
  })

  test("says when nothing matches", async () => {
    const { user } = await renderApp("/")
    await screen.findByRole("heading", { name: "Home" })
    await user.keyboard("{Control>}k{/Control}")
    await user.type(await screen.findByPlaceholderText(/Search pages/), "zzzzzz")
    expect(await screen.findByText("Nothing found.")).toBeInTheDocument()
  })

  test("the command menu button of the narrow header opens it", async () => {
    const { user } = await renderApp("/")
    await user.click(await screen.findByRole("button", { name: "Open command menu" }))
    expect(await screen.findByRole("dialog")).toBeInTheDocument()
  })
})
