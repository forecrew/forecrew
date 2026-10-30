import type { QueryClient } from "@tanstack/react-query"
import {
  createRootRouteWithContext, createRoute, createRouter, Outlet, redirect, type RouterHistory,
} from "@tanstack/react-router"
import { ApiError, setUnauthorizedHandler } from "@/api/client"
import { meOptions } from "@/api/queries"
import { AppShell } from "@/components/AppShell"
import { NotFound, RouteError } from "@/components/RouteError"
import { GoalPage } from "@/routes/goal"
import { GoalsPage, NewGoalPage } from "@/routes/goals"
import { HomePage } from "@/routes/home"
import { InboxPage } from "@/routes/inbox"
import { JobPage } from "@/routes/job"
import { JobsPage } from "@/routes/jobs"
import { LoginPage } from "@/routes/login"
import { ProjectPage } from "@/routes/project"
import { OnboardPage, ProjectsPage } from "@/routes/projects"
import { WorkersPage } from "@/routes/workers"

const root = createRootRouteWithContext<{ queryClient: QueryClient }>()({ component: Outlet })

// The query string of the list pages (filters) and of the login page (where to
// return to). One shape for all of them keeps the router's types simple.
interface Search {
  project?: string
  status?: string
  redirect?: string
}

const search = (s: Record<string, unknown>): Search => {
  const out: Search = {}
  for (const k of ["project", "status", "redirect"] as const) {
    const v = s[k]
    if (typeof v === "string" && v) out[k] = v
  }
  return out
}

const login = createRoute({
  getParentRoute: () => root,
  path: "/login",
  validateSearch: search,
  component: LoginPage,
})

// Everything else needs a session: without one the user goes to the login page.
const app = createRoute({
  getParentRoute: () => root,
  id: "app",
  beforeLoad: async ({ context, location }) => {
    try {
      await context.queryClient.ensureQueryData(meOptions)
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) throw redirect({ to: "/login", search: { redirect: location.href } })
      throw err
    }
  },
  component: AppShell,
})

const child = <P extends string>(path: P, component: () => React.ReactNode) => createRoute({ getParentRoute: () => app, path, component })

const routeTree = root.addChildren([
  login,
  app.addChildren([
    child("/", HomePage),
    createRoute({ getParentRoute: () => app, path: "/inbox", validateSearch: search, component: InboxPage }),
    createRoute({ getParentRoute: () => app, path: "/goals", validateSearch: search, component: GoalsPage }),
    child("/goals/new", NewGoalPage),
    child("/goals/$ref", GoalPage),
    child("/projects", ProjectsPage),
    child("/projects/new", OnboardPage),
    child("/projects/$name", ProjectPage),
    createRoute({ getParentRoute: () => app, path: "/jobs", validateSearch: search, component: JobsPage }),
    child("/jobs/$ref", JobPage),
    child("/workers", WorkersPage),
  ]),
])

export function createAppRouter(queryClient: QueryClient, history?: RouterHistory) {
  const router = createRouter({ routeTree, history, context: { queryClient }, defaultPreload: false, defaultErrorComponent: RouteError, defaultNotFoundComponent: NotFound })
  // Every 401 goes back to the login page, remembering where the user was.
  setUnauthorizedHandler(() => {
    queryClient.clear()
    if (router.state.location.pathname === "/login") return
    void router.navigate({ to: "/login", search: { redirect: router.state.location.href } })
  })
  return router
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
