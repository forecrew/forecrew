import { Link, Outlet } from "@tanstack/react-router"
import { useMe } from "@/api/queries"
import { PageHeader } from "@/components/PageHeader"
import { SETTINGS, allowed } from "@/lib/permissions"

/** Settings: the sections the caller may read, on the left, and the section. */
export function SettingsLayout() {
  const me = useMe().data
  const sections = SETTINGS.filter((s) => allowed(me, s.permission))
  if (sections.length === 0) {
    return (
      <>
        <PageHeader title="Settings" />
        <p className="rounded-xl border border-dashed bg-card p-6 text-center text-muted-foreground">Only organization admins and owners can see settings.</p>
      </>
    )
  }
  return (
    <div className="grid gap-6 md:grid-cols-[12rem_1fr]">
      <nav aria-label="Settings sections" className="flex gap-1 overflow-x-auto md:flex-col">
        {sections.map((s) => (
          <Link
            key={s.to}
            to={s.to}
            activeOptions={{ exact: "exact" in s }}
            className="flex h-8 items-center whitespace-nowrap rounded-lg px-3 text-sm font-medium text-muted-foreground hover:bg-accent hover:text-foreground [&.active]:bg-accent [&.active]:text-foreground"
          >
            {s.label}
          </Link>
        ))}
      </nav>
      <div className="min-w-0">
        <Outlet />
      </div>
    </div>
  )
}

/** The page of a section: the caller needs its permission, else it is as if the page were not there. */
export function Section({ permission, title, meta, children, actions }: { permission: string; title: string; meta?: string; children: React.ReactNode; actions?: React.ReactNode }) {
  const me = useMe().data
  if (!me) return null
  if (!allowed(me, permission)) {
    return (
      <>
        <PageHeader title={title} />
        <p className="rounded-xl border border-dashed bg-card p-6 text-center text-muted-foreground">
          You cannot see this section: it needs <code className="font-mono">{permission}</code>.
        </p>
      </>
    )
  }
  return (
    <>
      <PageHeader title={title} meta={meta}>
        {actions}
      </PageHeader>
      {children}
    </>
  )
}
