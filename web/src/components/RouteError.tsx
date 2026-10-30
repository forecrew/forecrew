import { Link, useRouter, type ErrorComponentProps } from "@tanstack/react-router"
import { AlertCircleIcon } from "lucide-react"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"

/** Shown when a route fails to load for a reason other than a missing session. */
export function RouteError({ error }: ErrorComponentProps) {
  const router = useRouter()
  return (
    <main className="mx-auto max-w-xl p-4">
      <Alert variant="destructive">
        <AlertCircleIcon aria-hidden="true" />
        <AlertTitle>Something went wrong</AlertTitle>
        <AlertDescription>
          <p>{error instanceof Error ? error.message : "Unknown error"}</p>
          <Button variant="outline" className="mt-2" onClick={() => void router.invalidate()}>
            Try again
          </Button>
        </AlertDescription>
      </Alert>
    </main>
  )
}

export function NotFound() {
  return (
    <main className="mx-auto max-w-xl p-4">
      <h1 className="mb-2 text-2xl font-semibold">Page not found</h1>
      <p className="text-muted-foreground">
        There is nothing at this address.{" "}
        <Link to="/" className="text-primary underline underline-offset-2">
          Go to Home
        </Link>
      </p>
    </main>
  )
}
