import { useQueryClient } from "@tanstack/react-query"
import { Link, Outlet, useNavigate, useSearch } from "@tanstack/react-router"
import {
  BookOpenIcon, BriefcaseIcon, ChevronsUpDownIcon, CommandIcon, FlaskConicalIcon, GavelIcon, UsersIcon, FolderGitIcon, InboxIcon, LayoutDashboardIcon, MenuIcon, ScaleIcon, ServerIcon, SettingsIcon, TargetIcon,
} from "lucide-react"
import { useCallback, useState } from "react"
import { api } from "@/api/client"
import { useGates, useMe } from "@/api/queries"
import { CommandMenu, CommandMenuContext, useCommandMenuShortcut } from "@/components/CommandMenu"
import { ProjectSelect } from "@/components/ProjectSelect"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Label } from "@/components/ui/label"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"
import { SETTINGS, allowed } from "@/lib/permissions"
import { Sheet, SheetContent, SheetDescription, SheetTitle, SheetTrigger } from "@/components/ui/sheet"

const NAV = [
  { to: "/", label: "Home", icon: LayoutDashboardIcon },
  { to: "/inbox", label: "Inbox", icon: InboxIcon },
  { to: "/goals", label: "Goals", icon: TargetIcon },
  { to: "/councils", label: "Councils", icon: UsersIcon },
  { to: "/decisions", label: "Decisions", icon: GavelIcon },
  { to: "/projects", label: "Projects", icon: FolderGitIcon },
  { to: "/jobs", label: "Jobs", icon: BriefcaseIcon },
  { to: "/quality", label: "Quality", icon: FlaskConicalIcon },
  { to: "/assumptions", label: "Assumptions", icon: ScaleIcon },
  { to: "/knowledge", label: "Knowledge", icon: BookOpenIcon },
  { to: "/workers", label: "Workers", icon: ServerIcon },
] as const

// Pages that list things for one project; their sidebar links keep the chosen project.
const FILTERED = ["/inbox", "/goals", "/councils", "/decisions", "/jobs", "/quality", "/assumptions", "/knowledge"]

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

/**
 * The organization the session works in. With two or more it is a switcher
 * (only organizations the person is an active member of, and the session may
 * enter: one from an organization's own connection has just its own).
 */
function OrganizationSwitcher({ onNavigate }: { onNavigate?: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const me = useMe().data
  const orgs = me?.organizations ?? []
  const [error, setError] = useState("")
  if (!me?.account) return null
  if (orgs.length < 2) {
    return <p className="truncate px-1 text-sm font-medium">{me.account.name}</p>
  }
  const change = async (slug: string) => {
    setError("")
    try {
      await api.switchOrganization(slug)
      // Everything on screen belongs to the organization left behind.
      await qc.invalidateQueries()
      onNavigate?.()
      await navigate({ to: "/" })
    } catch (err) {
      setError((err as Error).message)
    }
  }
  return (
    <div className="space-y-1.5">
      <Label htmlFor="organization-switcher" className="sr-only">
        Switch organization
      </Label>
      <NativeSelect id="organization-switcher" className="w-full" value={me.account.slug} onChange={(e) => void change(e.target.value)}>
        {orgs.map((o) => (
          <NativeSelectOption key={o.id} value={o.slug}>
            {o.name}
          </NativeSelectOption>
        ))}
      </NativeSelect>
      {error ? (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      ) : null}
    </div>
  )
}

/** Settings, when the caller may read a section, and the account menu. */
function AccountBlock({ onNavigate }: { onNavigate?: () => void }) {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const me = useMe().data
  const settings = SETTINGS.find((s) => allowed(me, s.permission))
  const name = me?.user?.name ?? me?.token

  const signOut = async (everywhere: boolean) => {
    try {
      await (everywhere ? api.signOutEverywhere() : api.logout())
    } finally {
      qc.clear()
      await navigate({ to: "/login" })
    }
  }

  return (
    <div className="mt-auto space-y-2 border-t pt-4">
      {settings ? (
        <Link to={settings.to} onClick={onNavigate} className={linkClass}>
          <SettingsIcon className="size-4" aria-hidden="true" />
          Settings
        </Link>
      ) : null}
      <DropdownMenu>
        <DropdownMenuTrigger render={<Button variant="ghost" className="w-full justify-between gap-2" aria-label="Account menu" />}>
          <span className="truncate">{name}</span>
          <ChevronsUpDownIcon aria-hidden="true" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="min-w-52">
          <DropdownMenuItem render={<Link to="/account" onClick={onNavigate} />}>Your account</DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem onClick={() => void signOut(false)}>Sign out</DropdownMenuItem>
          <DropdownMenuItem onClick={() => void signOut(true)}>Sign out everywhere</DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}

/** The sidebar (a menu on narrow screens), the command menu and the page. */
export function AppShell() {
  const project = useProjectFilter()
  const [menu, setMenu] = useState(false)
  const [commands, setCommands] = useState(false)
  const toggleCommands = useCallback(() => setCommands((o) => !o), [])
  useCommandMenuShortcut(toggleCommands)

  return (
    <div className="min-h-screen md:grid md:grid-cols-[15rem_1fr]">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:bg-card focus:p-3">
        Skip to content
      </a>
      <aside className="hidden flex-col gap-4 border-r bg-sidebar p-4 text-sidebar-foreground md:sticky md:top-0 md:flex md:h-screen">
        <Brand />
        <OrganizationSwitcher />
        <ProjectSwitcher />
        <NavLinks />
        <AccountBlock />
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
              <OrganizationSwitcher onNavigate={() => setMenu(false)} />
              <ProjectSwitcher onNavigate={() => setMenu(false)} />
              <NavLinks onNavigate={() => setMenu(false)} />
              <AccountBlock onNavigate={() => setMenu(false)} />
            </SheetContent>
          </Sheet>
        </nav>
      </div>
      <CommandMenu open={commands} onOpenChange={setCommands} />
    </div>
  )
}
