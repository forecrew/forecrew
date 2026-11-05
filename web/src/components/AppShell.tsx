import { useQueryClient } from "@tanstack/react-query"
import { Link, Outlet, useNavigate, useSearch } from "@tanstack/react-router"
import {
  BriefcaseIcon, CommandIcon, GavelIcon, UsersIcon, FolderGitIcon, InboxIcon, KeyRoundIcon, LayoutDashboardIcon, LogOutIcon, MenuIcon, ServerIcon, TargetIcon,
} from "lucide-react"
import { useCallback, useState } from "react"
import { api } from "@/api/client"
import { useGates, useMe } from "@/api/queries"
import { CommandMenu, CommandMenuContext, useCommandMenuShortcut } from "@/components/CommandMenu"
import { ProjectSelect } from "@/components/ProjectSelect"
import { Button } from "@/components/ui/button"
import { Label } from "@/components/ui/label"
import { Sheet, SheetContent, SheetDescription, SheetTitle, SheetTrigger } from "@/components/ui/sheet"

const NAV = [
  { to: "/", label: "Home", icon: LayoutDashboardIcon },
  { to: "/inbox", label: "Inbox", icon: InboxIcon },
  { to: "/goals", label: "Goals", icon: TargetIcon },
  { to: "/councils", label: "Councils", icon: UsersIcon },
  { to: "/decisions", label: "Decisions", icon: GavelIcon },
  { to: "/projects", label: "Projects", icon: FolderGitIcon },
  { to: "/jobs", label: "Jobs", icon: BriefcaseIcon },
  { to: "/workers", label: "Workers", icon: ServerIcon },
] as const

// Pages that list things for one project; their sidebar links keep the chosen project.
const FILTERED = ["/inbox", "/goals", "/councils", "/decisions", "/jobs"]

/** The project filter in the address, "" for all projects. */
const useProjectFilter = () => ((useSearch({ strict: false }) as { project?: string }).project ?? "")

const linkClass =
  "flex h-8 items-center gap-2 rounded-lg px-3 text-sm font-medium text-muted-foreground hover:bg-accent hover:text-foreground [&.active]:bg-accent [&.active]:text-foreground"

function PendingCount() {
  const pending = useGates().data?.length ?? 0
  if (pending === 0) return null
  return (
    <span className="rounded-full bg-primary px-2 py-0.5 text-xs text-primary-foreground">
      <span aria-hidden="true">{pending}</span>
      <span className="sr-only">{pending} pending</span>
    </span>
  )
}

function NavLinks({ onNavigate }: { onNavigate?: () => void }) {
  const project = useProjectFilter()
  return (
    <nav aria-label="Main" className="flex flex-col gap-1">
      {NAV.map(({ to, label, icon: Icon }) => (
        <Link key={to} to={to} search={(FILTERED.includes(to) && project ? { project } : {}) as never} activeOptions={{ exact: to === "/" }} onClick={onNavigate} className={linkClass}>
          <Icon className="size-4" aria-hidden="true" />
          <span className="flex-1">{label}</span>
          {to === "/inbox" ? <PendingCount /> : null}
        </Link>
      ))}
    </nav>
  )
}

/** Picks the project the lists show. It stays on the page and sets the project filter in the address. */
function ProjectSwitcher({ onNavigate }: { onNavigate?: () => void }) {
  const navigate = useNavigate()
  const project = useProjectFilter()
  return (
    <div className="space-y-1.5">
      <Label htmlFor="project-switcher" className="sr-only">
        Switch project
      </Label>
      <ProjectSelect
        id="project-switcher"
        allLabel="All projects"
        className="w-full"
        value={project}
        onChange={(v) => {
          void navigate({ to: ".", search: ((prev: Record<string, unknown>) => ({ ...prev, project: v || undefined })) as never })
          onNavigate?.()
        }}
      />
    </div>
  )
}

function Brand() {
  return (
    <p className="flex items-center gap-2 px-1 text-lg font-semibold">
      <span aria-hidden="true" className="flex size-8 items-center justify-center rounded-lg bg-primary text-sm text-primary-foreground">
        F
      </span>
      Forecrew
    </p>
  )
}

/** The sidebar (a menu on narrow screens), the command menu and the page. */
export function AppShell() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const me = useMe()
  const project = useProjectFilter()
  const [menu, setMenu] = useState(false)
  const [commands, setCommands] = useState(false)
  const toggleCommands = useCallback(() => setCommands((o) => !o), [])
  useCommandMenuShortcut(toggleCommands)

  const logout = async () => {
    try {
      await api.logout()
    } finally {
      qc.clear()
      await navigate({ to: "/login" })
    }
  }

  const account = (onNavigate?: () => void) => (
    <div className="mt-auto space-y-2 border-t pt-4">
      <Link to="/tokens" onClick={onNavigate} className={linkClass}>
        <KeyRoundIcon className="size-4" aria-hidden="true" />
        Tokens
      </Link>
      <p className="px-3 text-xs text-muted-foreground">
        Signed in as <span className="font-mono">{me.data?.token}</span>
        {me.data ? <> on {me.data.account.name}</> : null}
      </p>
      <Button variant="ghost" className="w-full justify-start gap-2" onClick={() => void logout()}>
        <LogOutIcon aria-hidden="true" /> Log out
      </Button>
    </div>
  )

  return (
    <div className="min-h-screen md:grid md:grid-cols-[15rem_1fr]">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:bg-card focus:p-3">
        Skip to content
      </a>
      <aside className="hidden flex-col gap-4 border-r bg-sidebar p-4 text-sidebar-foreground md:sticky md:top-0 md:flex md:h-screen">
        <Brand />
        <ProjectSwitcher />
        <NavLinks />
        {account()}
      </aside>
      <div className="min-w-0">
        <header className="flex items-center justify-between border-b bg-card px-4 py-2 md:hidden">
          <p className="font-semibold">Forecrew</p>
          <Button variant="ghost" aria-label="Open command menu" onClick={toggleCommands}>
            <CommandIcon aria-hidden="true" />
          </Button>
        </header>
        <main id="main" tabIndex={-1} className="mx-auto w-full max-w-6xl p-4 pb-24 outline-none md:p-8">
          <CommandMenuContext value={toggleCommands}>
            <Outlet />
          </CommandMenuContext>
        </main>
        <nav aria-label="Quick" className="fixed inset-x-0 bottom-0 z-40 grid grid-cols-4 border-t bg-card md:hidden">
          {(["/", "/inbox", "/jobs"] as const).map((to) => (
            <Link key={to} to={to} search={(project && to !== "/" ? { project } : {}) as never} activeOptions={{ exact: to === "/" }} className="flex h-14 items-center justify-center gap-1 text-sm text-muted-foreground [&.active]:font-medium [&.active]:text-primary">
              {to === "/" ? "Home" : to === "/inbox" ? "Inbox" : "Jobs"}
              {to === "/inbox" ? <PendingCount /> : null}
            </Link>
          ))}
          <Sheet open={menu} onOpenChange={setMenu}>
            <SheetTrigger render={<Button variant="ghost" aria-label="Menu" className="h-14 rounded-none text-muted-foreground" />}>
              <MenuIcon aria-hidden="true" /> Menu
            </SheetTrigger>
            <SheetContent side="left" className="flex flex-col gap-4 p-4">
              <SheetTitle>Forecrew</SheetTitle>
              <SheetDescription className="sr-only">Go to a page</SheetDescription>
              <ProjectSwitcher onNavigate={() => setMenu(false)} />
              <NavLinks onNavigate={() => setMenu(false)} />
              {account(() => setMenu(false))}
            </SheetContent>
          </Sheet>
        </nav>
      </div>
      <CommandMenu open={commands} onOpenChange={setCommands} />
    </div>
  )
}
