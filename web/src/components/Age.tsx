import { age, until } from "@/lib/format"

/** How long ago a time was (or, with `future`, how long until it), with the exact time as a tooltip. */
export function Age({ iso, ago, future }: { iso?: string; ago?: boolean; future?: boolean }) {
  if (!iso) return <span>never</span>
  const text = future ? until(iso) : age(iso)
  return (
    <time dateTime={iso} title={new Date(iso).toLocaleString()}>
      {ago && !future && text !== "just now" ? `${text} ago` : text}
    </time>
  )
}
