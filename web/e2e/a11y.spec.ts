import AxeBuilder from "@axe-core/playwright"
import { expect, test } from "@playwright/test"
import { mockApi } from "./api"

const pages: { name: string; path: string; ready: RegExp | string; loggedIn?: boolean; gates?: string; me?: string }[] = [
  { name: "login", path: "/login", ready: "Log in to Forecrew", loggedIn: false },
  { name: "home", path: "/", ready: "Home" },
  { name: "inbox", path: "/inbox", ready: "Inbox" },
  { name: "goals", path: "/goals", ready: "Goals" },
  { name: "new goal", path: "/goals/new", ready: "New goal" },
  { name: "goal", path: "/goals/GOAL-3", ready: /GOAL-3/ },
  { name: "inbox with a council brief", path: "/inbox?gate=GATE-12", ready: "Inbox", gates: "gates-council" },
  { name: "inbox with an assumption review", path: "/inbox?gate=GATE-13", ready: "Inbox", gates: "gates-council" },
  { name: "inbox with a job question", path: "/inbox?gate=GATE-14", ready: "Inbox", gates: "gates-council" },
  { name: "councils", path: "/councils", ready: "Councils" },
  { name: "start a council", path: "/councils/new", ready: "Start a council" },
  { name: "running council", path: "/councils/COUNCIL-2", ready: "COUNCIL-2" },
  { name: "decided council", path: "/councils/COUNCIL-3", ready: "COUNCIL-3" },
  { name: "failed council", path: "/councils/COUNCIL-4", ready: "COUNCIL-4" },
  { name: "decisions", path: "/decisions", ready: "Decisions" },
  { name: "projects", path: "/projects", ready: "Projects" },
  { name: "onboard", path: "/projects/new", ready: "Onboard repository" },
  { name: "project", path: "/projects/app", ready: "app" },
  { name: "jobs", path: "/jobs", ready: "Jobs" },
  { name: "job", path: "/jobs/JOB-41", ready: /JOB-41/ },
  { name: "quality", path: "/quality", ready: "Quality" },
  { name: "workers", path: "/workers", ready: "Workers" },
  { name: "assumptions", path: "/assumptions", ready: "Assumptions" },
  { name: "knowledge", path: "/knowledge?project=app", ready: "Knowledge" },
  { name: "knowledge proposals", path: "/knowledge?project=app&tab=proposals", ready: "Knowledge" },
  { name: "knowledge notes", path: "/knowledge?project=app&tab=notes", ready: "Knowledge" },
  { name: "knowledge integrations", path: "/knowledge?project=app&tab=integrations", ready: "Knowledge" },
  { name: "trace", path: "/trace/TASK-12", ready: "Why TASK-12?" },
  { name: "your account", path: "/account", ready: "Your account", me: "me-admin" },
  { name: "allow the command line", path: "/cli-login?redirect_uri=http%3A%2F%2F127.0.0.1%3A5555%2Fcallback&state=s&code_challenge=c&host=laptop", ready: "Allow the command line", me: "me-admin" },
  { name: "settings general", path: "/settings", ready: "General", me: "me-admin" },
  { name: "settings members", path: "/settings/people", ready: "People", me: "me-admin" },
  { name: "settings teams", path: "/settings/people?tab=teams", ready: "People", me: "me-admin" },
  { name: "settings roles", path: "/settings/people?tab=roles", ready: "People", me: "me-admin" },
  { name: "settings service accounts", path: "/settings/people?tab=service-accounts", ready: "People", me: "me-admin" },
  { name: "settings members for a viewer", path: "/settings/people", ready: "People", me: "me-viewer" },
  { name: "settings sign-in", path: "/settings/sign-in", ready: "Sign-in", me: "me-admin" },
  { name: "settings change history", path: "/settings/history", ready: "Change history", me: "me-admin" },
  { name: "project access", path: "/projects/app", ready: "app", me: "me-admin" },
]

const sizes = [
  { name: "desktop", width: 1280, height: 800 },
  { name: "phone", width: 375, height: 812 },
]

for (const size of sizes) {
  for (const p of pages) {
    test(`${p.name} has no serious accessibility violations on ${size.name}`, async ({ page }) => {
      await page.setViewportSize({ width: size.width, height: size.height })
      await mockApi(page, { loggedIn: p.loggedIn ?? true, gates: p.gates, me: p.me })
      await page.goto(p.path)
      await expect(page.getByRole("heading", { level: 1, name: p.ready })).toBeVisible()
      // wait for the data of the page
      await page.waitForLoadState("networkidle")
      const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]).analyze()
      const bad = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical")
      expect(bad.map((v) => `${v.id}: ${v.help} (${v.nodes.map((n) => n.target.join(" ")).join(", ")})`)).toEqual([])
      // no horizontal scrolling of the page
      await expect
        .poll(() => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth))
        .toBeLessThanOrEqual(0)
    })
  }

  test(`the command menu and the dark theme have no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await page.emulateMedia({ colorScheme: "dark" })
    await mockApi(page)
    await page.goto("/inbox")
    await expect(page.getByRole("article", { name: /GATE-9/ })).toBeVisible()
    await page.keyboard.press("Control+k")
    await expect(page.getByPlaceholder(/Search pages/)).toBeVisible()
    await page.waitForLoadState("networkidle")
    const results = await new AxeBuilder({ page }).analyze()
    const bad = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical")
    expect(bad.map((v) => `${v.id}: ${v.help}`)).toEqual([])
  })
}
