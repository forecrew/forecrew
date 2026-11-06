import { cellLabel, countsText, envLabel, fileSize, resultTone, verificationTone, assumptionOfGate, councilOfGate, councilTone, cost, count, decisionRef, gateLabel, isQuestion, isRefOrId, levelTone, until, goalRef, goalTone, jobRef, jobTone, projectTone, splitLines, splitList, stageOf, taskTone, words } from "./format"
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

describe("council helpers", () => {
  test("read the council and the assumption a gate is about from its title", () => {
    expect(councilOfGate("COUNCIL-5: Which queue?")).toBe("COUNCIL-5")
    expect(councilOfGate("Which queue? COUNCIL-5: x")).toBeUndefined()
    expect(assumptionOfGate("Review A-7: Fewer than 50 workers.")).toBe("A-7")
    expect(assumptionOfGate("Approve A-7")).toBeUndefined()
  })

  test("tell a job's question from an escalation", () => {
    expect(isQuestion({ kind: "needs_you", origin: "ask_human" })).toBe(true)
    expect(isQuestion({ kind: "needs_you", origin: "playbook" })).toBe(false)
    expect(isQuestion({ kind: "needs_you" })).toBe(false)
    expect(isQuestion({ kind: "merge_approval", origin: "ask_human" })).toBe(false)
    expect(gateLabel({ kind: "needs_you", origin: "ask_human" })).toBe("Question")
    expect(gateLabel({ kind: "needs_you" })).toBe("Needs you")
    expect(gateLabel({ kind: "council_decision" })).toBe("Council decision")
  })

  test("tones, refs and lines", () => {
    expect(councilTone("decided")).toBe("good")
    expect(councilTone("failed")).toBe("bad")
    expect(councilTone("running")).toBe("attention")
    expect(councilTone("canceled")).toBe("neutral")
    expect(levelTone("high")).toBe("bad")
    expect(levelTone("medium")).toBe("attention")
    expect(levelTone("low")).toBe("neutral")
    expect(decisionRef(9)).toBe("DEC-9")
    expect(splitLines(" a \n\n b\n")).toEqual(["a", "b"])
  })
})

test("file sizes", () => {
  expect(fileSize(12)).toBe("12 B")
  expect(fileSize(1536)).toBe("1.5 KB")
  expect(fileSize(20480)).toBe("20 KB")
  expect(fileSize(1572864)).toBe("1.5 MB")
})

test("counts text leaves out zero counts but passed", () => {
  expect(countsText({ passed: 0, failed: 0, skipped: 0, errored: 0 })).toBe("0 passed")
  expect(countsText({ passed: 1200, failed: 2, skipped: 3, errored: 4 })).toBe("1,200 passed, 2 failed, 4 errored, 3 skipped")
})

test("cells and environments", () => {
  expect(cellLabel({ os: "linux", go: "1.26" })).toBe("go=1.26, os=linux")
  expect(cellLabel({})).toBe("")
  expect(envLabel("")).toBe("host")
  expect(envLabel("linux")).toBe("linux")
})

test("verification and result tones", () => {
  expect(verificationTone("passed")).toBe("good")
  expect(verificationTone("failed")).toBe("bad")
  expect(verificationTone("incomplete")).toBe("attention")
  expect([resultTone("passed"), resultTone("errored"), resultTone("missing"), resultTone("skipped")]).toEqual(["good", "bad", "attention", "neutral"])
})
