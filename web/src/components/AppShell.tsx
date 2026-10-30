import { useQueryClient } from "@tanstack/react-query"
import { Link, Outlet, useNavigate } from "@tanstack/react-router"
import {
  BriefcaseIcon, FolderGitIcon, InboxIcon, LogOutIcon, MenuIcon, ServerIcon, TargetIcon,
} from "lucide-react"
import { useState } from "react"
import { api } from "@/api/client"
import { useGates, useMe } from "@/api/queries"
import { Button } from "@/components/ui/button"
import { Sheet, SheetContent, SheetDescription, SheetTitle, SheetTrigger } from "@/components/ui/sheet"

const NAV = [
  { to: "/inbox", label: "Inbox", icon: InboxIcon },
  { to: "/goals", label: "Goals", icon: TargetIcon },
  { to: "/projects", label: "Projects", icon: FolderGitIcon },
  { to: "/jobs", label: "Jobs", icon: BriefcaseIcon },
  { to: "/workers", label: "Workers", icon: ServerIcon },
] as const

function NavLinks({ onNavigate }: { onNavigate?: () => void }) {
  const pending = useGates().data?.length ?? 0
  return (
    <nav aria-label="Main" className="flex flex-col gap-1">
      {NAV.map(({ to, label, icon: Icon }) => (
        <Link
          key={to}
          to={to}
          activeOptions={{ exact: false }}
          onClick={onNavigate}
          className="flex min-h-11 items-center gap-3 rounded-lg px-3 text-sm font-medium text-muted-foreground hover:bg-accent hover:text-foreground [&.active]:bg-accent [&.active]:text-foreground"
        >
          <Icon className="size-4" aria-hidden="true" />
          <span className="flex-1">{label}</span>
          {to === "/inbox" && pending > 0 ? (
            <span className="rounded-full bg-primary px-2 py-0.5 text-xs text-primary-foreground">
              <span aria-hidden="true">{pending}</span>
              <span className="sr-only">{pending} pending</span>
            </span>
          ) : null}
        </Link>
      ))}
    </nav>
  )
}

/** The sidebar (a menu on narrow screens), the command menu and the page. */
export function AppShell() {
  const qc = useQueryClient()
  const navigate = useNavigate()
  const me = useMe()
  const [menu, setMenu] = useState(false)

  const logout = async () => {
    try {
      await api.logout()
    } finally {
      qc.clear()
      await navigate({ to: "/login" })
    }
  }

  const account = (
    <div className="mt-auto space-y-2 border-t pt-4">
      <p className="px-3 text-xs text-label">
        Signed in as <span className="font-mono">{me.data?.token}</span>
        {me.data ? <> on {me.data.account.name}</> : null}
      </p>
      <Button variant="ghost" className="w-full justify-start gap-3" onClick={() => void logout()}>
        <LogOutIcon aria-hidden="true" /> Log out
      </Button>
    </div>
  )

  return (
    <div className="min-h-screen md:grid md:grid-cols-[15rem_1fr]">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:z-50 focus:bg-card focus:p-3">
        Skip to content
      </a>
      <aside className="hidden flex-col gap-6 border-r bg-card p-4 md:sticky md:top-0 md:flex md:h-screen">
        <p className="px-3 text-lg font-semibold">Forecrew</p>
        <NavLinks />
        {account}
      </aside>
      <div className="min-w-0">
        <header className="flex items-center justify-between border-b bg-card px-4 py-2 md:hidden">
          <Sheet open={menu} onOpenChange={setMenu}>
            <SheetTrigger asChild>
              <Button variant="ghost" aria-label="Open menu">
                <MenuIcon aria-hidden="true" />
              </Button>
            </SheetTrigger>
            <SheetContent side="left" className="flex flex-col gap-6 p-4">
              <SheetTitle>Forecrew</SheetTitle>
              <SheetDescription className="sr-only">Go to a page</SheetDescription>
              <NavLinks onNavigate={() => setMenu(false)} />
              {account}
            </SheetContent>
          </Sheet>
          <p className="font-semibold">Forecrew</p>
        </header>
        <main id="main" tabIndex={-1} className="mx-auto w-full max-w-5xl p-4 outline-none md:p-8">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
