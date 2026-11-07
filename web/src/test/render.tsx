import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from "@tanstack/react-router"
import { render } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { act } from "react"
import { createAppRouter } from "@/router"

/** Renders the whole app at a path with a fresh cache and in-memory history. */
export async function renderApp(path = "/") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0, gcTime: Infinity } } })
  const router = createAppRouter(queryClient, createMemoryHistory({ initialEntries: [path] }))
  const user = userEvent.setup()
  const view = render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
  await act(async () => {
    await router.load()
  })
  return { ...view, router, queryClient, user }
}

/** Renders one component inside a router and a fresh cache, for tests of a piece of a page. */
export async function renderPiece(ui: React.ReactNode) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 0, gcTime: Infinity } } })
  const root = createRootRoute({ component: () => <>{ui}</> })
  const index = createRoute({ getParentRoute: () => root, path: "/" })
  const router = createRouter({ routeTree: root.addChildren([index]), history: createMemoryHistory({ initialEntries: ["/"] }) })
  const user = userEvent.setup()
  const view = render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
  await act(async () => {
    await router.load()
  })
  return { ...view, queryClient, user }
}
