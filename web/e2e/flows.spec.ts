import { expect, test } from "@playwright/test"
import { mockApi } from "./api"

test("sign in through GitHub, open the inbox and approve a gate", async ({ page }) => {
  const calls = await mockApi(page, { loggedIn: false })
  await page.goto("/inbox")
  await expect(page).toHaveURL(/\/login\?redirect=%2Finbox/)
  // The fake GitHub signs in and sends the browser back to the page it came from.
  await page.getByRole("link", { name: "Sign in with github" }).click()
  await expect(page).toHaveURL(/\/inbox$/)

  await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible()
  const gate = page.getByRole("article", { name: /GATE-9/ })
  await expect(gate).toContainText("Merge approval")
  await gate.getByLabel("Note (optional)").fill("Ship it")
  await gate.getByRole("button", { name: "Approve" }).click()
  await expect(gate).toHaveCount(0)
  await expect(page.getByRole("article")).toHaveCount(1)
  await expect(page.getByRole("article", { name: /GATE-10/ })).toBeVisible()

  const approve = calls.log.find((c) => c.call === "POST /gates/GATE-9/approve")
  expect(approve?.body).toEqual({ feedback: "Ship it" })
  expect(calls.log.some((c) => c.call === "POST /session")).toBe(false) // no pasted token
})

test("keyboard shortcuts in the inbox", async ({ page }) => {
  await mockApi(page)
  await page.goto("/inbox")
  const list = page.getByRole("list", { name: "Pending decisions" })
  await expect(list.getByRole("button", { name: /Merge PR #9/ })).toHaveAttribute("aria-current", "true")
  await expect(page.getByRole("article", { name: /GATE-9/ })).toBeVisible()
  await page.keyboard.press("j")
  await expect(list.getByRole("button", { name: /Approve the spec/ })).toHaveAttribute("aria-current", "true")
  await expect(page.getByRole("article", { name: /GATE-10/ })).toBeVisible()
  await page.keyboard.press("r")
  await expect(page.getByLabel("What should change?")).toBeFocused()
})

test("create a new goal", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/goals/new")
  await page.getByLabel("Project", { exact: true }).selectOption("app")
  await page.getByLabel("Title").fill("Dark mode")
  await page.getByLabel("Description").fill("Follow the system setting.")
  await page.getByLabel("Platforms").fill("web")
  await page.getByRole("button", { name: "Create goal" }).click()
  await expect(page).toHaveURL(/\/goals\/GOAL-4$/)
  expect(calls.log.find((c) => c.call === "POST /goals")?.body).toEqual({
    project: "app", title: "Dark mode", body: "Follow the system setting.", platforms: ["web"],
  })
})

test("open a job and see its log", async ({ page }) => {
  await mockApi(page)
  await page.goto("/jobs")
  await page.getByRole("link", { name: /JOB-41/ }).click()
  await expect(page.getByRole("heading", { name: "JOB-41 · developer" })).toBeVisible()
  const log = page.getByRole("log", { name: "Job log" })
  await expect(log).toContainText("I will start with the mailer.")
  await expect(log).toContainText("Bash returned: ok")
})

test("cancel a running job from its page", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/jobs/JOB-41")
  await page.getByRole("button", { name: "Cancel job" }).click()
  const dialog = page.getByRole("dialog", { name: "Cancel JOB-41?" })
  await expect(dialog).toContainText("within about 30 seconds")
  await dialog.getByRole("button", { name: "Cancel JOB-41" }).click()
  await expect(page.getByRole("dialog")).toHaveCount(0)
  await expect(page.getByText("canceled", { exact: true })).toBeVisible()
  await expect(page.getByRole("button", { name: "Cancel job" })).toHaveCount(0)
  expect(calls.log.find((c) => c.call === "POST /jobs/JOB-41/cancel")).toBeDefined()
})

test("cancel a job from the jobs list with the keyboard", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/jobs")
  await page.getByRole("button", { name: "Cancel JOB-42" }).focus()
  await page.keyboard.press("Enter")
  await expect(page.getByRole("dialog", { name: "Cancel JOB-42?" })).toBeVisible()
  await page.getByRole("button", { name: "Cancel JOB-42" }).last().focus()
  await page.keyboard.press("Enter")
  await expect(page.getByRole("dialog")).toHaveCount(0)
  expect(calls.log.find((c) => c.call === "POST /jobs/JOB-42/cancel")).toBeDefined()
})

test("revoke another token on the account page", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/account")
  const row = page.getByRole("row", { name: /old-laptop/ })
  await row.getByRole("button", { name: "Revoke old-laptop" }).click()
  await page.getByRole("dialog", { name: "Revoke old-laptop?" }).getByRole("button", { name: "Revoke old-laptop" }).click()
  await expect(page.getByRole("dialog")).toHaveCount(0)
  await expect(row).toContainText("revoked")
  await expect(row.getByRole("button")).toHaveCount(0)
  expect(calls.log.some((c) => c.call.startsWith("POST /tokens/") && c.call.endsWith("/revoke"))).toBe(true)
})

test("revoking the token in use warns and goes to the login page", async ({ page }) => {
  await mockApi(page)
  await page.goto("/account")
  await expect(page.getByRole("row", { name: /laptop/ }).first()).toContainText("this session")
  await page.getByRole("button", { name: "Revoke laptop", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "Revoke laptop?" })
  await expect(dialog).toContainText("You will be logged out")
  await dialog.getByRole("button", { name: "Revoke laptop" }).click()
  await expect(page).toHaveURL(/\/login/)
  await expect(page.getByLabel("Email")).toBeVisible()
})

test("agent text with HTML is shown as text and the page's policy blocks scripts", async ({ page }) => {
  await mockApi(page)
  const violations: string[] = []
  page.on("console", (m) => {
    if (m.text().includes("Content Security Policy")) violations.push(m.text())
  })
  await page.goto("/jobs/JOB-41")
  const log = page.getByRole("log", { name: "Job log" })
  await expect(log).toContainText("<img src=x onerror=alert(1)> done")
  await expect(log.locator("img")).toHaveCount(0)
  expect(violations).toEqual([])
})

test("the command menu opens with Ctrl+K and jumps to a goal", async ({ page }) => {
  await mockApi(page)
  await page.goto("/")
  await page.keyboard.press("Control+k")
  await page.getByPlaceholder(/Search pages/).fill("GOAL-3")
  await page.getByRole("option", { name: /^GOAL-3 / }).click()
  await expect(page).toHaveURL(/\/goals\/GOAL-3$/)
})

// Base UI locks the page's scroll by setting styles on the body from script,
// which the Content-Security-Policy allows; no policy violation may appear.
test("dialogs lock the page's scroll without a policy violation", async ({ page }) => {
  await mockApi(page)
  const violations: string[] = []
  page.on("console", (m) => {
    if (m.text().includes("Content Security Policy")) violations.push(m.text())
  })
  // Base UI sets and clears the lock in a timeout after the dialog changes, so
  // the body's overflow is polled instead of read once.
  const overflow = () => expect.poll(() => page.evaluate(() => getComputedStyle(document.body).overflow))

  await page.goto("/inbox")
  await expect(page.getByRole("article", { name: /GATE-9/ })).toBeVisible()
  await overflow().toBe("visible")
  await page.keyboard.press("Control+k")
  await expect(page.getByPlaceholder(/Search pages/)).toBeVisible()
  await overflow().toBe("hidden")
  await page.keyboard.press("Escape")
  await expect(page.getByPlaceholder(/Search pages/)).toHaveCount(0)
  await overflow().toBe("visible")

  await page.setViewportSize({ width: 375, height: 812 })
  await page.getByRole("button", { name: "Menu", exact: true }).click()
  await expect(page.getByRole("dialog")).toBeVisible()
  await overflow().toBe("hidden")
  await page.keyboard.press("Escape")
  await expect(page.getByRole("dialog")).toHaveCount(0)
  await overflow().toBe("visible")
  expect(violations).toEqual([])
})

test("the goal page lists its jobs and approves a waiting gate", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/goals/GOAL-3")
  const jobs = page.getByRole("region", { name: "Jobs table" })
  await expect(jobs.getByRole("link", { name: /JOB-41/ })).toBeVisible()
  expect(calls.log.some((c) => c.call === "GET /jobs?goal=GOAL-3&limit=500")).toBe(true)
  const gate = page.getByRole("article", { name: /GATE-9/ })
  await gate.getByRole("button", { name: "Approve" }).click()
  await expect.poll(() => calls.log.some((c) => c.call === "POST /gates/GATE-9/approve")).toBe(true)
})

test("run an ad-hoc job with the advanced fields", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/projects/app")
  await page.getByLabel("Prompt").fill("Review the sign-in change.")
  await page.getByText("Advanced").click()
  await page.getByLabel("Goal", { exact: true }).fill("GOAL-3")
  await page.getByLabel("Branch").fill("agent/task-12")
  await page.getByLabel("Include repository facts").check()
  await page.getByRole("button", { name: "Run job" }).click()
  await expect(page).toHaveURL(/\/jobs\/JOB-42$/)
  expect(calls.log.find((c) => c.call === "POST /jobs")?.body).toMatchObject({
    project: "app", role: "reviewer", goal: "GOAL-3", branch: "agent/task-12", include_repo_facts: true,
  })
})

test("onboard a repository with a name and default branch", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/projects/new")
  await page.getByLabel("Repository").fill("acme/docs")
  await page.getByLabel("Name").fill("handbook")
  await page.getByLabel("Default branch").fill("trunk")
  await page.getByRole("button", { name: "Onboard repository" }).click()
  await expect(page).toHaveURL(/\/projects\//)
  expect(calls.log.find((c) => c.call === "POST /projects")?.body).toEqual({ repo: "acme/docs", name: "handbook", default_branch: "trunk" })
})

test("an admin invites someone and sees the link once", async ({ page }) => {
  const calls = await mockApi(page, { me: "me-admin" })
  await page.goto("/settings/people")
  await expect(page.getByRole("heading", { name: "People", level: 1 })).toBeVisible()
  await expect(page.getByText("Marta Rossi")).toBeVisible()
  await page.getByRole("button", { name: "Invite" }).click()
  const dialog = page.getByRole("dialog")
  await dialog.getByLabel("Email address").fill("new@acme.test")
  await dialog.getByRole("button", { name: "Create invitation" }).click()
  const card = page.getByRole("region", { name: "Invitation for new@acme.test" })
  await expect(card.getByLabel("Invitation link")).toHaveValue(/\/login\?invite=fci_/)
  await card.getByRole("button", { name: "Done" }).click()
  await expect(card).toHaveCount(0)
  expect(calls.log.find((c) => c.call === "POST /invitations")?.body).toMatchObject({ email: "new@acme.test", roles: [{ role: "member" }] })
})

test("a viewer reads the members but cannot invite", async ({ page }) => {
  await mockApi(page, { me: "me-viewer" })
  await page.goto("/settings/people")
  await expect(page.getByText("Marta Rossi")).toBeVisible()
  await expect(page.getByRole("button", { name: "Invite" })).toBeDisabled()
  await expect(page.getByText("member.manage")).toBeVisible()
  await expect(page.getByRole("link", { name: "Sign-in" })).toHaveCount(0)
})

test("Gate F: ask the assistant for a server, confirm its plan, see the machine and its job", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/machines")
  await expect(page.getByText("No machines yet. Agents need somewhere to work.")).toBeVisible()

  await page.getByRole("button", { name: "Ask", exact: true }).click()
  const panel = page.getByRole("region", { name: "Assistant" })
  await panel.getByLabel("Say what you want").fill("Add my Linux server")
  await panel.getByRole("button", { name: "Send" }).click()

  // the plan is a card the server drew; typing yes would confirm nothing
  const plan = panel.getByRole("region", { name: /Change plan: Add machine types to Home server/ })
  await expect(plan).toContainText("Machine type linux-small")
  await expect(plan).toContainText("Free. It is your machine.")
  await plan.getByRole("button", { name: "Confirm" }).click()
  await expect(plan.getByText("Applied")).toBeVisible()
  expect(calls.log.some((c) => c.call.startsWith("POST /config/plans/") && c.call.endsWith("/apply"))).toBe(true)

  // the machine is shown on the page beside the panel, and its job runs
  await expect(page.getByRole("link", { name: "Home Server" })).toBeVisible()
  const running = page.getByRole("region", { name: "Running jobs table" })
  await expect(running).toContainText("linux-small-8f2")
  await running.getByRole("link", { name: /JOB-41/ }).click()
  await expect(page.getByRole("heading", { name: "JOB-41 · developer" })).toBeVisible()
})

test("connect Linear: the plan names the token, which goes to its secure field and nowhere else", async ({ page }) => {
  const calls = await mockApi(page, { me: "me-admin" })
  await page.goto("/settings/integrations")
  await page.getByRole("button", { name: "Connect a tracker" }).click()
  const dialog = page.getByRole("dialog", { name: "Connect a tracker" })
  await dialog.getByLabel("Tracker", { exact: true }).selectOption("linear")
  await dialog.getByLabel("Teams").fill("ENG")
  await dialog.getByRole("button", { name: "Review the change" }).click()
  const field = dialog.getByRole("form", { name: "Secure field: token of tracker connection linear" })
  await field.getByLabel("token of tracker connection linear").fill("lin_api_exampleexampleexample")
  await field.getByRole("button", { name: "Save" }).click()
  await expect(field.getByText("token of tracker connection linear: Saved")).toBeVisible()
  const plan = calls.log.find((c) => c.call === "POST /config/plans")
  expect(plan?.body).toMatchObject({ source: "form", changes: [{ kind: "tracker_connection", name: "linear-2", spec: { kind: "linear", config: { teams: ["ENG"], token_ref: "store:linear-2-token" } } }] })
  expect(JSON.stringify(calls.log.filter((c) => !c.call.startsWith("PUT")))).not.toContain("lin_api_")
  expect(calls.log.find((c) => c.call.startsWith("PUT /config/plans/"))?.body).toEqual({ value: "lin_api_exampleexampleexample" })
})

test("accept a ticket that waits with no project, from the tracker screen", async ({ page }) => {
  const calls = await mockApi(page, { me: "me-admin" })
  await page.goto("/settings/integrations")
  await page.getByRole("button", { name: "Open the waiting tickets of jira" }).click()
  await page.getByLabel("Project for OPS-3").selectOption("app")
  await page.getByRole("button", { name: "Accept: OPS-3" }).click()
  await expect.poll(() => calls.log.find((c) => c.call.endsWith("/accept") && c.call.includes("951"))?.body).toEqual({ project: "app" })
})

test("add a key, its check passes, pick a preset, confirm, and a job runs on the new connection", async ({ page }) => {
  const calls = await mockApi(page, { me: "me-admin", noModels: true })
  await page.goto("/settings/models")
  await expect(page.getByText("Add an AI provider key so agents can work. One is enough.")).toBeVisible()
  await page.getByRole("button", { name: "Add provider" }).click()
  const dialog = page.getByRole("dialog", { name: "Add an AI provider" })
  await dialog.getByRole("radio", { name: /DeepSeek API/ }).check()
  await dialog.getByRole("button", { name: "Review the change" }).click()
  const field = dialog.getByRole("form", { name: "Secure field: API key of deepseek" })
  await field.getByLabel("API key of deepseek").fill("sk-exampleexampleexample")
  await field.getByRole("button", { name: "Save" }).click()
  await expect(field.getByText("API key of deepseek: Saved")).toBeVisible()
  await dialog.getByRole("button", { name: "Confirm" }).click()
  await page.getByRole("dialog", { name: /Confirm: Add AI provider/ }).getByLabel(/Type .* to confirm/).fill("acme")
  await page.getByRole("button", { name: "Confirm and apply" }).click()
  await dialog.getByRole("button", { name: "Done" }).click()
  // Saving checks the key: the connection shows that it works.
  const deepseek = page.getByRole("listitem", { name: "DeepSeek" })
  await expect(deepseek.getByText("Works")).toBeVisible()
  await page.getByRole("radiogroup", { name: "Preset" }).getByRole("radio", { name: "Low cost" }).click()
  const plan = page.getByRole("region", { name: "Change plan: Models suggested (low_cost)" })
  await plan.getByRole("button", { name: "Confirm" }).click()
  await page.getByLabel(/Type .* to confirm/).fill("acme")
  await page.getByRole("button", { name: "Confirm and apply" }).click()
  await expect(plan.getByText(/Applied/).first()).toBeVisible()
  await page.goto("/jobs/JOB-41")
  await expect(page.getByText("model deepseek-chat")).toBeVisible()
  // The key went to its secure field and nowhere else.
  expect(JSON.stringify(calls.log.filter((c) => !c.call.startsWith("PUT")))).not.toContain("sk-example")
  expect(calls.log.find((c) => c.call.startsWith("PUT /config/plans/"))?.body).toEqual({ value: "sk-exampleexampleexample" })
})
