import { expect, test } from "@playwright/test"
import { mockApi } from "./api"

test("log in, open the inbox and approve a gate", async ({ page }) => {
  const calls = await mockApi(page, { loggedIn: false })
  await page.goto("/inbox")
  await expect(page).toHaveURL(/\/login\?redirect=%2Finbox/)
  await page.getByLabel("API token").fill("fcm_example")
  await page.getByRole("button", { name: "Log in" }).click()

  await expect(page.getByRole("heading", { name: "Inbox" })).toBeVisible()
  const gate = page.getByRole("article", { name: /GATE-9/ })
  await expect(gate).toContainText("Merge approval")
  await gate.getByLabel("Note (optional)").fill("Ship it")
  await gate.getByRole("button", { name: "Approve" }).click()
  await expect(gate).toHaveCount(0)
  await expect(page.getByRole("article")).toHaveCount(2)

  const approve = calls.log.find((c) => c.call === "POST /gates/GATE-9/approve")
  expect(approve?.body).toEqual({ feedback: "Ship it" })
  expect(calls.log.find((c) => c.call === "POST /session")?.body).toEqual({ token: "fcm_example" })
})

test("keyboard shortcuts in the inbox", async ({ page }) => {
  await mockApi(page)
  await page.goto("/inbox")
  await expect(page.getByRole("article", { name: /GATE-9/ })).toHaveAttribute("aria-current", "true")
  await page.keyboard.press("j")
  await expect(page.getByRole("article", { name: /GATE-10/ })).toHaveAttribute("aria-current", "true")
  await page.keyboard.press("r")
  await expect(page.getByLabel("What should change?")).toBeFocused()
})

test("create a new goal", async ({ page }) => {
  const calls = await mockApi(page)
  await page.goto("/goals/new")
  await page.getByLabel("Project").selectOption("app")
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

// react-remove-scroll (inside Radix dialogs) always tries to add an inline <style>
// for its scroll lock, which the policy blocks. That one known violation is
// allowed; index.css has a static rule that does the locking instead, which the
// test checks. Any other violation fails.
test("dialogs lock the page's scroll with the policy's only known violation", async ({ page }) => {
  await mockApi(page)
  const violations: string[] = []
  page.on("console", (m) => {
    if (m.text().includes("Content Security Policy")) violations.push(m.text())
  })
  const overflow = () => page.evaluate(() => getComputedStyle(document.body).overflow)

  await page.goto("/inbox")
  await expect(page.getByRole("article", { name: /GATE-9/ })).toBeVisible()
  expect(await overflow()).toBe("visible")
  await page.keyboard.press("Control+k")
  await expect(page.getByPlaceholder(/Search pages/)).toBeVisible()
  expect(await overflow()).toBe("hidden")
  await page.keyboard.press("Escape")
  await expect(page.getByPlaceholder(/Search pages/)).toHaveCount(0)
  expect(await overflow()).toBe("visible")

  await page.setViewportSize({ width: 375, height: 812 })
  await page.getByRole("button", { name: "Open menu" }).click()
  await expect(page.getByRole("dialog")).toBeVisible()
  expect(await overflow()).toBe("hidden")
  await page.keyboard.press("Escape")
  await expect(page.getByRole("dialog")).toHaveCount(0)
  expect(violations.filter((v) => !v.startsWith("Applying inline style violates"))).toEqual([])
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
