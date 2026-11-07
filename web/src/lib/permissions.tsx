import { useId } from "react"
import { useMe } from "@/api/queries"
import type { Permission, Session } from "@/api/types"
import { Button } from "@/components/ui/button"

/**
 * Whether the caller holds a permission: on the organization, or on a project
 * (an organization role covers every project). The server decides in the end;
 * the UI hides or disables what it would refuse.
 */
export function allowed(me: Session | undefined, permission: Permission, project?: string): boolean {
  if (!me) return false
  if (me.permissions?.includes(permission)) return true
  return project ? Boolean(me.projects?.[project]?.includes(permission)) : false
}

/** Whether the caller holds the permission on some project. */
export function allowedAnywhere(me: Session | undefined, permission: Permission): boolean {
  return allowed(me, permission) || Object.values(me?.projects ?? {}).some((p) => p.includes(permission))
}

/** `can(permission, projectId)` for the signed-in caller; false until /me is known. */
export function useCan() {
  const me = useMe().data
  return (permission: Permission, project?: string) => allowed(me, permission, project)
}

/** Whether the caller holds the permission on the organization or on some project. */
export function useCanAnywhere() {
  const me = useMe().data
  return (permission: Permission) => allowedAnywhere(me, permission)
}

/** The sections of Settings, each with the permission that shows it. */
export const SETTINGS = [
  { to: "/settings", label: "General", permission: "org.read", exact: true },
  { to: "/settings/people", label: "People", permission: "org.read" },
  { to: "/settings/sign-in", label: "Sign-in", permission: "connection.manage" },
  { to: "/settings/models", label: "AI models", permission: "models.read" },
  { to: "/settings/integrations", label: "Integrations", permission: "trackers.read" },
  { to: "/settings/policies", label: "Policies", permission: "worker.read", or: "billing.manage" },
  { to: "/settings/history", label: "Change history", permission: "audit.read", or: "config.apply" },
] as const

/** Whether the caller may see a section of Settings: its permission, or the one that reads part of it (Billing reads budgets). */
export function sectionAllowed(me: Session | undefined, s: { permission: Permission; or?: Permission }): boolean {
  return allowed(me, s.permission) || (s.or ? allowed(me, s.or) : false)
}

type ButtonProps = React.ComponentProps<typeof Button>

/**
 * A button for an action that needs a permission. Without it the button is
 * disabled and says which permission is missing, in words and in a tooltip;
 * where the caller may not even read, the page leaves the button out.
 */
export function PermButton({ permission, project, children, ...props }: ButtonProps & { permission: Permission; project?: string }) {
  const can = useCan()
  const id = useId()
  if (can(permission, project)) return <Button {...props}>{children}</Button>
  return (
    <span className="inline-flex flex-col items-start gap-0.5" title={`Needs ${permission}`}>
      <Button {...props} disabled aria-describedby={id}>
        {children}
      </Button>
      <span id={id} className="text-xs text-muted-foreground">
        Needs <code className="font-mono">{permission}</code>
      </span>
    </span>
  )
}
