import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { Age } from "./Age"
import { ExternalLink } from "./ExternalLink"
import { Field } from "./Field"
import { Markdown } from "./Markdown"
import { PageHeader } from "./PageHeader"
import { StatusBadge } from "./StatusBadge"
import { TableWrap } from "./TableWrap"

describe("Markdown", () => {
  test("renders markdown", () => {
    render(<Markdown>{"## Title\n\n- one\n- two\n\n`code` and **bold**"}</Markdown>)
    expect(screen.getByRole("heading", { name: "Title" })).toBeInTheDocument()
    expect(screen.getAllByRole("listitem")).toHaveLength(2)
    expect(screen.getByText("code").tagName).toBe("CODE")
  })

  test("shows raw HTML as text and never interprets it", () => {
    const { container } = render(
      <Markdown>{'<script>alert(1)</script>\n\n<img src="x" onerror="alert(1)">\n\n<a href="javascript:alert(1)">x</a>\n\n<div onclick="x()">hi</div>'}</Markdown>,
    )
    expect(container.querySelector("script, img, div[onclick], a[href^=javascript]")).toBeNull()
    expect(container).toHaveTextContent("<script>alert(1)</script>")
  })

  test("drops images and does not link javascript: URLs", () => {
    const { container } = render(<Markdown>{"![pixel](https://evil.example/p.png) [bad](javascript:alert(1))"}</Markdown>)
    expect(container.querySelector("img")).toBeNull()
    expect(container.querySelector("a")?.getAttribute("href") ?? "").not.toMatch(/^javascript:/)
  })

  test("opens links in a new tab safely", () => {
    render(<Markdown>{"[docs](https://example.com)"}</Markdown>)
    const a = screen.getByRole("link", { name: "docs" })
    expect(a).toHaveAttribute("target", "_blank")
    expect(a).toHaveAttribute("rel", "noopener noreferrer")
  })
})

test("ExternalLink opens in a new tab and says so", () => {
  render(<ExternalLink href="https://github.com/acme/app">Repo</ExternalLink>)
  const a = screen.getByRole("link", { name: /^Repo\s*\(opens in a new tab\)$/ })
  expect(a).toHaveAttribute("href", "https://github.com/acme/app")
  expect(a).toHaveAttribute("rel", "noopener noreferrer")
})

describe("Age", () => {
  test.each([
    ["2026-03-01T11:59:50Z", "just now"],
    ["2026-03-01T11:15:00Z", "45 min"],
    ["2026-03-01T09:00:00Z", "3 h"],
    ["2026-02-26T12:00:00Z", "3 d"],
  ])("%s is %s", (iso, text) => {
    render(<Age iso={iso} />)
    expect(screen.getByText(text)).toHaveAttribute("datetime", iso)
  })

  test("can say ago, and never", () => {
    const { rerender } = render(<Age iso="2026-03-01T11:15:00Z" ago />)
    expect(screen.getByText("45 min ago")).toBeInTheDocument()
    rerender(<Age iso="2026-03-01T11:59:59Z" ago />)
    expect(screen.getByText("just now")).toBeInTheDocument()
    rerender(<Age />)
    expect(screen.getByText("never")).toBeInTheDocument()
  })
})

describe("Field", () => {
  test("labels its control and links hint and error", () => {
    render(
      <Field label="Name" hint="Your name" error="Required">
        {(p) => <input {...p} />}
      </Field>,
    )
    const input = screen.getByLabelText("Name")
    expect(input).toHaveAttribute("aria-invalid", "true")
    expect(input).toHaveAccessibleDescription("Your name Required")
  })

  test("without hint or error has no description", () => {
    render(<Field label="Name">{(p) => <input {...p} />}</Field>)
    expect(screen.getByLabelText("Name")).not.toHaveAttribute("aria-describedby")
    expect(screen.getByLabelText("Name")).toHaveAttribute("aria-invalid", "false")
  })
})

test("StatusBadge shows its words", () => {
  render(<StatusBadge tone="bad">failing</StatusBadge>)
  expect(screen.getByText("failing")).toBeInTheDocument()
})

test("PageHeader has the page's only h1 and its actions", () => {
  render(
    <PageHeader title="Goals">
      <button>New</button>
    </PageHeader>,
  )
  expect(screen.getByRole("heading", { level: 1, name: "Goals" })).toBeInTheDocument()
  expect(screen.getByRole("button", { name: "New" })).toBeInTheDocument()
  render(<PageHeader title="Alone" />)
  expect(screen.getByRole("heading", { name: "Alone" })).toBeInTheDocument()
})

test("TableWrap is a focusable, labelled region", async () => {
  render(
    <TableWrap label="Jobs">
      <p>x</p>
    </TableWrap>,
  )
  const region = screen.getByRole("region", { name: "Jobs table" })
  await userEvent.tab()
  expect(region).toHaveFocus()
})
