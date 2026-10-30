import type { UseQueryResult } from "@tanstack/react-query"
import { AlertCircleIcon } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"

interface Props<T> {
  query: UseQueryResult<T>
  /** Shown instead of children when this returns true. */
  isEmpty?: (data: T) => boolean
  empty?: React.ReactNode
  children: (data: T) => React.ReactNode
}

/** The loading, error and empty states of a query, then its data. */
export function Async<T>({ query, isEmpty, empty, children }: Props<T>) {
  if (query.isPending) {
    return (
      <div role="status" aria-live="polite" className="space-y-3">
        <span className="sr-only">Loading…</span>
        <Skeleton className="h-16 w-full" />
        <Skeleton className="h-16 w-full" />
      </div>
    )
  }
  if (query.isError) {
    return (
      <Alert variant="destructive">
        <AlertCircleIcon aria-hidden="true" />
        <AlertTitle>Could not load this</AlertTitle>
        <AlertDescription>
          <p>{query.error.message}</p>
          <Button variant="outline" className="mt-2" onClick={() => void query.refetch()}>
            Try again
          </Button>
        </AlertDescription>
      </Alert>
    )
  }
  if (isEmpty?.(query.data)) {
    return <p className="rounded-xl border border-dashed bg-card p-6 text-center text-muted-foreground">{empty}</p>
  }
  return <>{children(query.data)}</>
}
