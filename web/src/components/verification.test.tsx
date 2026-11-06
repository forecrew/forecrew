import { render, screen, within } from "@testing-library/react"
import type { VerificationInfo } from "@/api/types"
import { fx } from "@/test/server"
import { VerificationPanel } from "./Verification"

const base = fx.verification as unknown as VerificationInfo

describe("verification panel", () => {
  test("shows status, blocking reasons, environments, traceability and the report link", () => {
    render(<VerificationPanel info={base} title="PR 9" />)
    const panel = screen.getByRole("region", { name: "PR 9" })
    expect(within(panel).getByRole("heading", { name: "Verification" }).parentElement).toHaveTextContent("failedabc1234")
    expect(within(panel).getByText("abc1234")).toBeInTheDocument()
    expect(within(panel).queryByText(/stale/)).not.toBeInTheDocument()
    expect(within(panel).getByRole("list", { name: "Blocking reasons" })).toHaveTextContent("e2e failed in linux")
    expect(within(panel).getByText(/Environments:/).parentElement).toHaveTextContent("linux (go go1.26.1, node v24.1.0)")
    const table = within(panel).getByRole("region", { name: "PR 9 criteria table" })
    const ac3 = within(table).getByText("AC-3").closest("tr")!
    expect(within(ac3).getByText("untested")).toBeInTheDocument()
    expect(within(ac3).getByText("no tests")).toBeInTheDocument()
    const ac1 = within(table).getByText("AC-1").closest("tr")!
    expect(within(ac1).getAllByText("passed")).toHaveLength(2)
    expect(within(ac1).getByText("auth::TestLinkSent")).toBeInTheDocument()
    expect(within(panel).getByRole("link", { name: "Download report" })).toHaveAttribute("href", `/api/v1/artifacts/${base.artifact_id}`)
  })

  test("marks a report of an earlier commit and leaves out what it does not have", () => {
    const report = { ...base.report, blocking: [], flaky: [], environments: { "": { versions: {} } }, criteria: { "AC-1": { status: "untested", tests: {}, untestable: "visual design" } } }
    const info = { ...base, status: "passed", current: false, artifact_id: undefined, report } as unknown as VerificationInfo
    render(<VerificationPanel info={info} title="PR 9" />)
    expect(screen.getByText("stale: not the latest commit")).toBeInTheDocument()
    expect(screen.getByText("passed")).toBeInTheDocument()
    expect(screen.queryByRole("link", { name: "Download report" })).not.toBeInTheDocument()
    expect(screen.queryByRole("list", { name: "Blocking reasons" })).not.toBeInTheDocument()
    expect(screen.queryByRole("list", { name: "Flaky tests" })).not.toBeInTheDocument()
    expect(screen.getByText("host")).toBeInTheDocument()
    expect(screen.getByText("untestable: visual design")).toBeInTheDocument()
    // a criterion with a reason is not a gap
    expect(screen.getByText("Criteria: 0 of 1 covered")).toBeInTheDocument()
  })

  test("a report with no criteria and no environments is only a status", () => {
    const report = { ...base.report, criteria: {}, environments: {}, blocking: [], flaky: [] }
    render(<VerificationPanel info={{ ...base, report }} title="PR 9" />)
    expect(screen.queryByText(/Criteria:/)).not.toBeInTheDocument()
    expect(screen.queryByText(/Environments:/)).not.toBeInTheDocument()
  })
})
