/** Plain scalars that YAML would read as something else, or that need quotes. */
const PLAIN = /^[A-Za-z_][A-Za-z0-9_./@-]*$/
const RESERVED = /^(true|false|null|yes|no|on|off|~)$/i

function scalar(v: unknown): string {
  if (v === null || v === undefined) return "null"
  if (typeof v === "string") return PLAIN.test(v) && !RESERVED.test(v) ? v : JSON.stringify(v)
  return String(v)
}

function lines(v: unknown, indent: string): string[] {
  if (Array.isArray(v)) {
    if (v.length === 0) return [`${indent}[]`]
    return v.flatMap((item) => {
      if (item !== null && typeof item === "object") {
        const inner = lines(item, indent + "  ")
        return [`${indent}- ${inner[0]!.trimStart()}`, ...inner.slice(1)]
      }
      return [`${indent}- ${scalar(item)}`]
    })
  }
  if (v !== null && typeof v === "object") {
    const entries = Object.entries(v as Record<string, unknown>)
    if (entries.length === 0) return [`${indent}{}`]
    return entries.flatMap(([k, val]) => {
      if (val !== null && typeof val === "object" && !(Array.isArray(val) ? val.length === 0 : Object.keys(val).length === 0)) {
        return [`${indent}${k}:`, ...lines(val, indent + "  ")]
      }
      return [`${indent}${k}: ${lines(val, "")[0]}`]
    })
  }
  return [`${indent}${scalar(v)}`]
}

/** A value as YAML, for showing resources to people; it is not meant to be parsed back. */
export function toYaml(v: unknown): string {
  return lines(v, "").join("\n") + "\n"
}
