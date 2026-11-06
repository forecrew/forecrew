import { PAGE } from "@/api/client"

/** Says so when a list has the most the API gives, because there may be more. */
export function Capped({ n }: { n: number }) {
  if (n < PAGE) return null
  return <p className="col-span-full text-sm text-muted-foreground">Showing the first {PAGE}. Narrow the list to see others.</p>
}
