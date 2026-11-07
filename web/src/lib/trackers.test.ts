import { addressOf, checkHelp, configList, kindLabel, looksPrivate, reasonWords, routeSentence, safeUrl, scopesOf, suggestStates, triggerWords } from "./trackers"
import type { TrackerConnection } from "@/api/types"

const conn = (kind: string, config: Record<string, unknown>): TrackerConnection => ({ id: "1", name: "c", kind, config, triggers: {}, created_at: "", updated_at: "" })

test("names a connection by where it points", () => {
  expect(addressOf(conn("jira", { base_url: "https://jira.acme.local/jira" }))).toBe("jira.acme.local")
  expect(addressOf(conn("jira", { base_url: "not a url" }))).toBe("not a url")
  expect(addressOf(conn("jira", {}))).toBe("")
  expect(addressOf(conn("linear", { teams: ["ENG", "OPS"] }))).toBe("teams ENG, OPS")
  expect(addressOf(conn("github", { repos: ["acme/app"] }))).toBe("acme/app")
  expect(addressOf(conn("github_project", { owner: "acme", project: 7 }))).toBe("acme board 7")
  expect(addressOf(conn("other", {}))).toBe("")
  expect(kindLabel("github_project")).toBe("GitHub Projects")
  expect(kindLabel("other")).toBe("other")
  expect(configList(conn("linear", { teams: "ENG" }), "teams")).toEqual([])
  expect(scopesOf(conn("jira", { projects: ["ENG"] }))).toEqual(["ENG"])
  expect(scopesOf(conn("github", { repos: ["a/b"] }))).toEqual(["a/b"])
  expect(scopesOf(conn("github_project", {}))).toEqual([])
})

test("says triggers and rules as sentences", () => {
  expect(triggerWords({ label: "forecrew", state: "Ready", assignee: "u1" })).toBe("labeled forecrew or assigned to u1 or moved to Ready")
  expect(routeSentence({ match: { project: "ENG", labels: ["a", "b"], types: ["Bug"], components: ["api"] }, project: "app" })).toBe("Tickets from ENG of type Bug labeled a or b in api go to app")
  expect(routeSentence({ match: {}, project: "app" })).toBe("Tickets from any project go to app")
})

test("says why a ticket waits", () => {
  expect(reasonWords({ status: "waiting", reason: "unknown_person", handed_by: "Marta" })).toBe("Marta isn't in Forecrew. Start this anyway?")
  expect(reasonWords({ status: "waiting", reason: "unknown_person" })).toMatch(/^Whoever handed this over/)
  expect(reasonWords({ status: "waiting", reason: "no_route" })).toMatch(/No rule/)
  expect(reasonWords({ status: "waiting", reason: "many_projects" })).toMatch(/more than one project/)
  expect(reasonWords({ status: "waiting", reason: "not_permitted" })).toMatch(/can't create goals/)
  expect(reasonWords({ status: "waiting" })).toMatch(/Waiting for a person/)
  expect(reasonWords({ status: "queued", limit: 3 })).toMatch(/3 goals/)
  expect(reasonWords({ status: "queued" })).toMatch(/free slot/)
})

test("links only web addresses", () => {
  expect(safeUrl("https://x.test/a")).toBe("https://x.test/a")
  expect(safeUrl("HTTP://x.test")).toBe("HTTP://x.test")
  expect(safeUrl("javascript:alert(1)")).toBeUndefined()
  expect(safeUrl(undefined)).toBeUndefined()
})

test("suggests statuses from the names the tracker has", () => {
  expect(suggestStates([{ name: "To Do" }, { name: "In Progress" }, { name: "Code Review" }, { name: "Done", closed: true }, { name: "Won't Do", closed: true }])).toEqual({
    in_progress: "In Progress", in_review: "Code Review", done: "Done", canceled: "Won't Do",
  })
  // Without a name that says done, the first closed one that is not a cancel stands for it.
  expect(suggestStates([{ name: "Open" }, { name: "Rejected", closed: true }, { name: "Finito", closed: true }])).toEqual({ done: "Finito", canceled: "Rejected" })
  expect(suggestStates([])).toEqual({})
  // An open status is never a finished one.
  expect(suggestStates([{ name: "Done for now" }])).toEqual({})
})

test("knows addresses a hosted server cannot reach", () => {
  for (const url of ["https://localhost", "https://jira.local/x", "https://wiki.internal", "https://jira", "https://10.1.2.3", "https://172.20.0.1", "https://192.168.1.5", "https://127.0.0.1:8080", "https://169.254.1.1", "https://[::1]", "https://[fd00::1]", "https://a.lan", "https://x.corp", "https://x.intranet"]) {
    expect(looksPrivate(url), url).toBe(true)
  }
  for (const url of ["https://acme.atlassian.net", "https://172.32.0.1", "https://8.8.8.8", "nonsense", ""]) {
    expect(looksPrivate(url), url).toBe(false)
  }
})

test("tells what to do about each check", () => {
  expect(checkHelp("address", "address 10.0.0.1 is private: refused")).toMatch(/hosted server/)
  expect(checkHelp("address", "no such host")).toMatch(/Check it/)
  expect(checkHelp("address", undefined)).toMatch(/Check it/)
  for (const n of ["certificate", "login", "projects", "transitions", "routes"]) expect(checkHelp(n, "x")).not.toBe("")
  expect(checkHelp("other", "x")).toBe("")
})
