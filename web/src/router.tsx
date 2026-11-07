import type { QueryClient } from "@tanstack/react-query"
import {
  createRootRouteWithContext, createRoute, createRouter, Outlet, redirect, type RouterHistory,
} from "@tanstack/react-router"
import { ApiError, setUnauthorizedHandler } from "@/api/client"
import { meOptions } from "@/api/queries"
import { AppShell } from "@/components/AppShell"
import { NotFound, RouteError } from "@/components/RouteError"
import { AccountPage } from "@/routes/account"
import { CliLoginPage } from "@/routes/cli-login"
import { CouncilPage, CouncilsPage, NewCouncilPage } from "@/routes/councils"
import { DecisionsPage } from "@/routes/decisions"
import { AssumptionsPage } from "@/routes/assumptions"
import { ProposalsPage } from "@/routes/proposals"
import { GoalPage } from "@/routes/goal"
import { GoalsPage, NewGoalPage } from "@/routes/goals"
import { HomePage } from "@/routes/home"
import { InboxPage } from "@/routes/inbox"
import { JobPage } from "@/routes/job"
import { JobsPage } from "@/routes/jobs"
import { KnowledgePage } from "@/routes/knowledge"
import { LoginPage } from "@/routes/login"
import { SetupLinkPage } from "@/routes/setup-link"
import { QualityPage } from "@/routes/quality"
import { ProjectPage } from "@/routes/project"
import { OnboardPage, ProjectsPage } from "@/routes/projects"
import { SetupPage } from "@/routes/setup"
import { TracePage } from "@/routes/trace"
import { GeneralPage } from "@/routes/settings/general"
import { HistoryPage } from "@/routes/settings/history"
import { SettingsLayout } from "@/routes/settings/layout"
import { PeoplePage } from "@/routes/settings/people"
import { PoliciesPage } from "@/routes/settings/policies"
import { SignInPage } from "@/routes/settings/sign-in"
import { AddMachinesPage } from "@/routes/machines/add"
import { MachinesPage } from "@/routes/machines"
import { LocationPage } from "@/routes/machines/location"

const root = createRootRouteWithContext<{ queryClient: QueryClient }>()({ component: Outlet })

// The query string of the list pages (filters) and of the login page (where to
// return to). One shape for all of them keeps the router's types simple.
interface Search {
  project?: string
  status?: string
  redirect?: string
  /** The sign-in page: the organization of /login?org= and the invitation link. */
  org?: string
  invite?: string
  /** The decision the inbox opens with (a Home link). */
  gate?: string
  /** The question a new council opens with. */
  question?: string
  impact?: string
  category?: string
  /** The knowledge page's tab, search words and filters. */
  tab?: string
  q?: string
  section?: string
  kind?: string
  provider?: string
  goal?: string
  /** The plan the inbox opens with (a link from an MCP client). */
  plan?: string
  /** The title a new goal starts with. */
  title?: string
  /** What forecrew login asks the browser to allow. */
  redirect_uri?: string
  state?: string
  code_challenge?: string
  organization?: string
  host?: string
}

const search = (s: Record<string, unknown>): Search => {
  const out: Search = {}
  for (const k of ["project", "status", "redirect", "org", "invite", "gate", "question", "impact", "category", "tab", "redirect_uri", "state", "code_challenge", "organization", "host", "q", "section", "kind", "provider", "goal", "title", "plan"] as const) {
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

// The one-time setup link of a new server: no session yet, and the token stays in the address's #.
const setupLink = createRoute({ getParentRoute: () => root, path: "/setup-link", component: SetupLinkPage })

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

// Every page accepts the project filter, so the sidebar switcher can set it where the user is.
const child = <P extends string>(path: P, component: () => React.ReactNode) =>
  createRoute({ getParentRoute: () => app, path, validateSearch: search, component })

const settings = createRoute({ getParentRoute: () => app, path: "/settings", validateSearch: search, component: SettingsLayout })

const routeTree = root.addChildren([
  login,
  setupLink,
  app.addChildren([
    child("/", HomePage),
    createRoute({ getParentRoute: () => app, path: "/inbox", validateSearch: search, component: InboxPage }),
    createRoute({ getParentRoute: () => app, path: "/goals", validateSearch: search, component: GoalsPage }),
    createRoute({ getParentRoute: () => app, path: "/goals/new", validateSearch: search, component: NewGoalPage }),
    createRoute({ getParentRoute: () => app, path: "/councils", validateSearch: search, component: CouncilsPage }),
    child("/councils/new", NewCouncilPage),
    child("/councils/$ref", CouncilPage),
    createRoute({ getParentRoute: () => app, path: "/decisions", validateSearch: search, component: DecisionsPage }),
    child("/goals/$ref", GoalPage),
    child("/projects", ProjectsPage),
    child("/projects/new", OnboardPage),
    child("/projects/$name", ProjectPage),
    createRoute({ getParentRoute: () => app, path: "/assumptions", validateSearch: search, component: AssumptionsPage }),
    createRoute({ getParentRoute: () => app, path: "/proposals", validateSearch: search, component: ProposalsPage }),
    createRoute({ getParentRoute: () => app, path: "/knowledge", validateSearch: search, component: KnowledgePage }),
    createRoute({ getParentRoute: () => app, path: "/jobs", validateSearch: search, component: JobsPage }),
    child("/jobs/$ref", JobPage),
    child("/quality", QualityPage),
    createRoute({ getParentRoute: () => app, path: "/machines", validateSearch: search, component: MachinesPage }),
    createRoute({ getParentRoute: () => app, path: "/machines/add", validateSearch: search, component: AddMachinesPage }),
    child("/machines/$id", LocationPage),
    // Workers became Machines.
    createRoute({ getParentRoute: () => app, path: "/workers", validateSearch: search, beforeLoad: () => { throw redirect({ to: "/machines" }) } }),
    child("/trace/$ref", TracePage),
    createRoute({ getParentRoute: () => app, path: "/setup", validateSearch: search, component: SetupPage }),
    // Tokens moved into the account page.
    createRoute({ getParentRoute: () => app, path: "/tokens", validateSearch: search, beforeLoad: () => { throw redirect({ to: "/account" }) } }),
    child("/account", AccountPage),
    child("/cli-login", CliLoginPage),
    settings.addChildren([
      createRoute({ getParentRoute: () => settings, path: "/", validateSearch: search, component: GeneralPage }),
      createRoute({ getParentRoute: () => settings, path: "/people", validateSearch: search, component: PeoplePage }),
      createRoute({ getParentRoute: () => settings, path: "/sign-in", validateSearch: search, component: SignInPage }),
      createRoute({ getParentRoute: () => settings, path: "/policies", validateSearch: search, component: PoliciesPage }),
      createRoute({ getParentRoute: () => settings, path: "/history", validateSearch: search, component: HistoryPage }),
    ]),
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
