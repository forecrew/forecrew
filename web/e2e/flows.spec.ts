import { expect, test } from "@playwright/test"
import { mockApi } from "./api"

test("log in, open the inbox and approve a gate", async ({ page }) => {
  const calls = await mockApi(page, { loggedIn: false })
  await page.goto("/inbox")
  await expect(page).toHaveURL(/\/login\?redirect=%2Finbox/)
  await expect(page.getByRole("link", { name: "Sign in with github" })).toHaveAttribute("href", /^\/auth\/.+\/login$/)
  await page.getByLabel("API token").fill("fcm_example")
  await page.getByRole("button", { name: "Log in" }).click()

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
  expect(calls.log.find((c) => c.call === "POST /session")?.body).toEqual({ token: "fcm_example" })
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

test("revoke another token on the tokens page", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/tokens")
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
  await page.goto("/tokens")
  await expect(page.getByRole("row", { name: /laptop/ }).first()).toContainText("this session")
  await page.getByRole("button", { name: "Revoke laptop", exact: true }).click()
  const dialog = page.getByRole("dialog", { name: "Revoke laptop?" })
  await expect(dialog).toContainText("You will be logged out")
  await dialog.getByRole("button", { name: "Revoke laptop" }).click()
  await expect(page).toHaveURL(/\/login/)
  await expect(page.getByLabel("API token")).toBeVisible()
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
  await page.getByRole("option", { name: /GOAL-3/ }).click()
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
