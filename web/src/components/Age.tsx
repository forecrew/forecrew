import { age } from "@/lib/format"

/** How long ago a time was, with the exact time as a tooltip. */
export function Age({ iso, ago }: { iso?: string; ago?: boolean }) {
  if (!iso) return <span>never</span>
  const text = age(iso)
  return (
    <time dateTime={iso} title={new Date(iso).toLocaleString()}>
      {ago && text !== "just now" ? `${text} ago` : text}
    </time>
  )
}
