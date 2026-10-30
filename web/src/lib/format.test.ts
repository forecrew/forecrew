import { cost, goalRef, goalTone, jobRef, jobTone, projectTone, splitList, stageOf, taskTone, words } from "./format"
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
