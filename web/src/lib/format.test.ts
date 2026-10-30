import { cost, count, isRefOrId, until, goalRef, goalTone, jobRef, jobTone, projectTone, splitList, stageOf, taskTone, words } from "./format"
import { safeRedirect } from "./redirect"

test("refs", () => {
  expect(goalRef(3)).toBe("GOAL-3")
  expect(jobRef(41)).toBe("JOB-41")
})

test("cost", () => {
  expect(cost(1.745)).toMatch(/^\$1\.7[45]$/)
  expect(cost(undefined)).toBe("$0.00")
})

test.each([
  ["draft", "spec"],
  ["spec_review", "spec"],
  ["planned", "planning"],
  ["in_progress", "building"],
  ["done", "done"],
  ["canceled", "canceled"],
] as const)("stage of %s is %s", (status, stage) => {
  expect(stageOf(status)).toBe(stage)
})

test("tones", () => {
  expect(goalTone("done")).toBe("good")
  expect(goalTone("spec_review")).toBe("attention")
  expect(goalTone("canceled")).toBe("neutral")
  expect(goalTone("draft")).toBe("neutral")
  expect(taskTone("merged")).toBe("good")
  expect(taskTone("in_review")).toBe("attention")
  expect(taskTone("closed")).toBe("neutral")
  expect(taskTone("open")).toBe("neutral")
  expect(jobTone("succeeded")).toBe("good")
  expect(jobTone("failed")).toBe("bad")
  expect(jobTone("queued")).toBe("attention")
  expect(jobTone("running")).toBe("neutral")
  expect(projectTone("onboarded")).toBe("good")
  expect(projectTone("onboarding")).toBe("attention")
  expect(projectTone("registered")).toBe("neutral")
})

test("words and lists", () => {
  expect(words("changes_requested")).toBe("changes requested")
  expect(splitList(" web, ios  android,,")).toEqual(["web", "ios", "android"])
  expect(splitList("")).toEqual([])
})

test.each([
  ["/goals?status=draft", "/goals?status=draft"],
  [undefined, "/"],
  ["", "/"],
  ["//evil.example", "/"],
  ["/\\evil.example", "/"],
  ["https://evil.example", "/"],
  ["goals", "/"],
])("safeRedirect(%j) = %s", (input, want) => {
  expect(safeRedirect(input)).toBe(want)
})

test("count and until", () => {
  expect(count(12345)).toBe("12,345")
  expect(count(undefined)).toBe("0")
  const now = Date.parse("2026-03-01T12:00:00Z")
  expect(until("2026-03-01T12:05:00Z", now)).toBe("in 5 min")
  expect(until("2026-03-01T11:00:00Z", now)).toBe("now")
  expect(until(undefined, now)).toBe("never")
})

test.each([
  ["GOAL", "GOAL-3", true],
  ["GOAL", "goal-12", true],
  ["GOAL", "GOAL-0", false],
  ["GOAL", "GOAL-", false],
  ["GOAL", "GOAL-3-4", false],
  ["GOAL", "TASK-3", false],
  ["TASK", "TASK-12", true],
  ["TASK", "018f0000-0000-7000-8000-000000000040", true],
  ["TASK", "018f0000-0000-7000-8000-00000000004", false],
  ["TASK", "", false],
] as const)("isRefOrId(%s, %s) is %s", (kind, s, ok) => {
  expect(isRefOrId(kind, s)).toBe(ok)
})
