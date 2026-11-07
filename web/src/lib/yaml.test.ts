import { appliesText, permissionWords, sourceText } from "./config"
import { toYaml } from "./yaml"

test("writes nested resources as YAML", () => {
  expect(toYaml({ kind: "provider", spec: { limits: { max_machines: 3 }, tags: {}, network_allow: ["a.test", "10.0.0.0/8"], on: "yes" } })).toBe(
    ['kind: provider', 'spec:', '  limits:', '    max_machines: 3', '  tags: {}', '  network_allow:', '    - a.test', '    - "10.0.0.0/8"', '  on: "yes"', ""].join("\n"),
  )
})

test("writes lists of objects, empty lists and nulls", () => {
  expect(toYaml({ items: [{ a: 1, b: "x y" }, { a: 2 }], none: [], nothing: null, flag: true })).toBe(
    ["items:", "  - a: 1", '    b: "x y"', "  - a: 2", "none: []", "nothing: null", "flag: true", ""].join("\n"),
  )
  expect(toYaml(["a", 2])).toBe("- a\n- 2\n")
})

test("says where a plan applies, what a permission is and where a plan came from", () => {
  expect(appliesText("now")).toBe("Applies now")
  expect(appliesText("pr:acme/app")).toBe("Opens a PR on acme/app")
  expect(permissionWords("worker.manage")).toBe("Manage machines")
  expect(permissionWords("unknown.thing")).toBe("unknown.thing")
  expect(sourceText({ source: "mcp", client: "Claude Desktop" })).toBe("Claude Desktop")
  expect(sourceText({ source: "mcp" })).toBe("an MCP client")
  expect(["assistant", "yaml", "cli", "form"].map((source) => sourceText({ source: source as never }))).toEqual(["assistant", "YAML", "CLI", "form"])
})
