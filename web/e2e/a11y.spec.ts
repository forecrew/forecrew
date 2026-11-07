import AxeBuilder from "@axe-core/playwright"
import { expect, test, type Page } from "@playwright/test"
import { fixture, mockApi } from "./api"

const pages: { name: string; path: string; ready: RegExp | string; loggedIn?: boolean; gates?: string; me?: string; roles?: string }[] = [
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
  { name: "machines", path: "/machines", ready: "Machines" },
  { name: "machine types", path: "/machines?tab=types", ready: "Machines" },
  { name: "machines for a member", path: "/machines", ready: "Machines", me: "me-contributor" },
  { name: "add machines", path: "/machines/add", ready: "Add machines" },
  { name: "add a computer", path: "/machines/add?kind=computer", ready: "Add machines" },
  { name: "add an AWS account", path: "/machines/add?kind=aws", ready: "Add machines" },
  { name: "location", path: "/machines/018f0000-0000-7000-8000-000000000700", ready: "Home Server" },
  { name: "offline location", path: "/machines/018f0000-0000-7000-8000-000000000701", ready: "Mac Mini" },
  { name: "set up", path: "/setup", ready: "Set up Forecrew" },
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
  { name: "settings custom roles", path: "/settings/people?tab=roles", ready: "People", me: "me-owner-enterprise", roles: "roles-custom" },
  { name: "settings service accounts", path: "/settings/people?tab=service-accounts", ready: "People", me: "me-admin" },
  { name: "settings members for a viewer", path: "/settings/people", ready: "People", me: "me-viewer" },
  { name: "settings sign-in", path: "/settings/sign-in", ready: "Sign-in", me: "me-admin" },
  { name: "settings change history", path: "/settings/history", ready: "Change history", me: "me-admin" },
  { name: "settings all activity", path: "/settings/history?tab=activity", ready: "Change history", me: "me-admin" },
  { name: "settings policies", path: "/settings/policies", ready: "Policies", me: "me-admin" },
  { name: "settings policies for a viewer", path: "/settings/policies", ready: "Policies", me: "me-viewer" },
  { name: "settings integrations", path: "/settings/integrations", ready: "Integrations", me: "me-admin" },
  { name: "settings integrations for a viewer", path: "/settings/integrations", ready: "Integrations", me: "me-viewer" },
  { name: "project access", path: "/projects/app", ready: "app", me: "me-admin" },
]

/** No serious or critical violation on the page as it is now (dialogs and sheets included). */
async function expectNoSeriousViolations(page: Page) {
  await page.waitForLoadState("networkidle")
  // A dialog that is still fading in fails the contrast check: let animations end.
  await page.evaluate(() => Promise.all(document.getAnimations().map((a) => a.finished)))
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]).analyze()
  const bad = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical")
  expect(bad.map((v) => `${v.id}: ${v.help} (${v.nodes.map((n) => n.target.join(" ")).join(", ")})`)).toEqual([])
}

const sizes = [
  { name: "desktop", width: 1280, height: 800 },
  { name: "phone", width: 375, height: 812 },
]

for (const size of sizes) {
  for (const p of pages) {
    test(`${p.name} has no serious accessibility violations on ${size.name}`, async ({ page }) => {
      await page.setViewportSize({ width: size.width, height: size.height })
      await mockApi(page, { loggedIn: p.loggedIn ?? true, gates: p.gates, me: p.me, roles: p.roles })
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

  test(`the assistant panel with a plan has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page)
    await page.goto("/machines")
    await page.getByRole("button", { name: "Ask", exact: true }).click()
    await page.getByLabel("Say what you want").fill("Add my Linux server")
    await page.getByRole("button", { name: "Send" }).click()
    await expect(page.getByRole("region", { name: /Change plan/ })).toBeVisible()
    await page.waitForLoadState("networkidle")
    const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]).analyze()
    const bad = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical")
    expect(bad.map((v) => `${v.id}: ${v.help} (${v.nodes.map((n) => n.target.join(" ")).join(", ")})`)).toEqual([])
  })

  test(`the second confirmation of a plan that lowers protection has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page, { lowering: true })
    await page.goto("/machines")
    await page.getByRole("button", { name: "Ask", exact: true }).click()
    await page.getByLabel("Say what you want").fill("Let jobs use Forecrew Cloud")
    await page.getByRole("button", { name: "Send" }).click()
    const plan = page.getByRole("region", { name: /Change plan/ })
    await expect(plan).toBeVisible()
    if (size.width >= 768) {
      await plan.getByRole("button", { name: "Confirm" }).click()
      await expect(page.getByRole("dialog", { name: /Confirm: / })).toBeVisible()
    } else {
      // a plan that lowers protection is not confirmed from a phone
      await expect(plan.getByText(/cannot be confirmed from a phone/)).toBeVisible()
    }
    await expectNoSeriousViolations(page)
  })

  test(`a plan viewed as YAML has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page)
    await page.goto("/machines")
    await page.getByRole("button", { name: "Ask", exact: true }).click()
    await page.getByLabel("Say what you want").fill("Add my Linux server")
    await page.getByRole("button", { name: "Send" }).click()
    await page.getByRole("button", { name: "View as YAML" }).click()
    await expect(page.getByRole("dialog", { name: /Add machine types/ })).toBeVisible()
    await expectNoSeriousViolations(page)
  })

  test(`the change history sheet with the revert card has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page, { me: "me-admin" })
    await page.goto("/settings/history")
    await page.getByRole("button", { name: "Add machine types to Home server" }).click()
    const sheet = page.getByRole("dialog", { name: "Add machine types to Home server" })
    await sheet.getByRole("button", { name: "Revert" }).click()
    await expect(sheet.getByRole("region", { name: /Change plan: Revert/ })).toBeVisible()
    await expectNoSeriousViolations(page)
  })

  test(`the Export and Import dialogs have no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page, { me: "me-admin" })
    await page.goto("/settings/history")
    await page.getByRole("button", { name: "Export YAML" }).click()
    await expect(page.getByRole("dialog", { name: "Export YAML" }).getByText(/name: home-server/)).toBeVisible()
    await expectNoSeriousViolations(page)
    await page.keyboard.press("Escape")
    await expect(page.getByRole("dialog")).toHaveCount(0)
    await page.getByRole("button", { name: "Import", exact: true }).click()
    await expect(page.getByRole("dialog", { name: "Import YAML" })).toBeVisible()
    await expectNoSeriousViolations(page)
  })

  test(`the tracker dialogs and results have no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page, { me: "me-admin", trackerFails: true })
    await page.goto("/settings/integrations")
    const card = page.getByRole("region", { name: /Jira jira/ })
    await expect(card.getByText("2 people not matched yet")).toBeVisible()
    // The people to match, the tickets that wait and the results of a failed test.
    await card.getByRole("button", { name: "Review the people of jira" }).click()
    await expect(page.getByRole("list", { name: "People not matched yet" })).toBeVisible()
    await card.getByRole("button", { name: "Open the waiting tickets of jira" }).click()
    await expect(page.getByRole("list", { name: "Tickets waiting" })).toBeVisible()
    await card.getByRole("button", { name: "Test connection of jira" }).click()
    await expect(card.getByRole("list", { name: "Test of jira" })).toBeVisible()
    await expect(card.getByRole("form", { name: "Add the company's certificate" })).toBeVisible()
    await expectNoSeriousViolations(page)
    // The connect form, with its private address note and its plan with the secure field.
    await page.getByRole("button", { name: "Connect a tracker" }).click()
    const dialog = page.getByRole("dialog", { name: "Connect a tracker" })
    await dialog.getByLabel("Address", { exact: true }).fill("https://jira.acme.local/jira")
    await expect(dialog.getByRole("note")).toBeVisible()
    await expectNoSeriousViolations(page)
    await dialog.getByLabel("Projects", { exact: true }).fill("ENG")
    await dialog.getByRole("button", { name: "Review the change" }).click()
    await expect(dialog.getByRole("form", { name: /Secure field: token of tracker connection/ })).toBeVisible()
    await expectNoSeriousViolations(page)
    await page.keyboard.press("Escape")
    // The rule form with the statuses under More options.
    await card.getByRole("button", { name: "Add a rule to jira" }).click()
    const rule = page.getByRole("dialog", { name: "Add a rule" })
    await rule.getByText("More options").click()
    await expect(rule.getByLabel("When the goal is done")).toBeVisible()
    await expectNoSeriousViolations(page)
  })

  test(`the ticket card of the Inbox has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    const gates = (fixture("gates") as { ref: string; kind: string }[]).map((g, i) => (i === 0 ? { ...g, ref: "GATE-15", kind: "ticket_intake", title: "Accept ticket ENG-21 from jira" } : g))
    await mockApi(page, { gates: "gates" })
    await page.route("**/api/v1/gates", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(gates) }))
    await page.goto("/inbox?gate=GATE-15")
    const card = page.getByRole("article", { name: /GATE-15/ })
    await expect(card.getByText("Marta Rossi isn't in Forecrew. Start this anyway?")).toBeVisible()
    await expectNoSeriousViolations(page)
  })

  test(`the machine type sheet has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page, { me: "me-admin" })
    await page.goto("/machines?tab=types")
    await page.getByRole("button", { name: "Linux Small" }).click()
    await expect(page.getByRole("dialog", { name: "Linux Small" })).toBeVisible()
    await expectNoSeriousViolations(page)
  })

  test(`the Inbox with plans has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page, { inboxPlans: true })
    await page.goto("/inbox")
    await expect(page.getByRole("region", { name: "Change plan: Add Windows test machine type" })).toBeVisible()
    await expectNoSeriousViolations(page)
  })

  test(`the banner of a waiting job has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page)
    await page.goto("/jobs/JOB-42")
    await expect(page.getByRole("status", { name: "Why it waits" })).toContainText("Only Mac Mini can run this")
    await expectNoSeriousViolations(page)
  })

  test(`the join flow after the machine reports has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page)
    await page.goto("/machines/add?kind=computer")
    await page.getByRole("button", { name: "Get the command" }).click()
    await expect(page.getByRole("heading", { name: "Checks" })).toBeVisible()
    await expect(page.getByRole("heading", { name: "Confirm this location" })).toBeVisible()
    await expectNoSeriousViolations(page)
  })

  test(`the error alert of a list that failed to load has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page, { me: "me-admin" })
    await page.route("**/api/v1/connected-apps", (route) => route.fulfill({ status: 500, contentType: "application/json", body: JSON.stringify({ error: { code: "internal", message: "The server could not list your apps" } }) }))
    await page.goto("/account")
    await expect(page.getByRole("alert").first()).toContainText("Could not load this")
    await expectNoSeriousViolations(page)
  })

  test(`the setup link page has no serious violations on ${size.name}`, async ({ page }) => {
    await page.setViewportSize({ width: size.width, height: size.height })
    await mockApi(page, { loggedIn: false })
    await page.route("**/auth/setup", (route) => route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ organization: "Acme Labs" }) }))
    await page.goto("/setup-link#fcu_secret")
    await expect(page.getByRole("link", { name: /Sign in with/ }).first()).toBeVisible()
    await expectNoSeriousViolations(page)
  })

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

test("the custom role editor has no serious accessibility violations", async ({ page }) => {
  await mockApi(page, { me: "me-owner-enterprise", roles: "roles-custom" })
  await page.goto("/settings/people?tab=roles")
  await page.getByRole("button", { name: "New custom role" }).click()
  await expect(page.getByRole("dialog", { name: "New custom role" })).toBeVisible()
  const results = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"]).analyze()
  const bad = results.violations.filter((v) => v.impact === "serious" || v.impact === "critical")
  expect(bad.map((v) => `${v.id}: ${v.help} (${v.nodes.map((n) => n.target.join(" ")).join(", ")})`)).toEqual([])
})
