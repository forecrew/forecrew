import { useNavigate, useSearch } from "@tanstack/react-router"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Section } from "./layout"
import { MembersTab } from "./members"
import { RolesTab } from "./roles"
import { ServiceAccountsTab } from "./service-accounts"
import { TeamsTab } from "./teams"

const TABS = [
  { value: "members", label: "Members" },
  { value: "teams", label: "Teams" },
  { value: "roles", label: "Roles" },
  { value: "service-accounts", label: "Service accounts" },
] as const

/** Members, teams, roles and service accounts, one tab at a time. */
export function PeoplePage() {
  const { tab } = useSearch({ strict: false }) as { tab?: string }
  const navigate = useNavigate()
  const current = TABS.find((t) => t.value === tab)?.value ?? "members"
  return (
    <Section permission="org.read" title="People">
      <Tabs value={current} onValueChange={(v) => void navigate({ to: "/settings/people", search: { tab: String(v) } as never })}>
        <TabsList aria-label="People">
          {TABS.map((t) => (
            <TabsTrigger key={t.value} value={t.value}>
              {t.label}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      <div className="mt-4">
        {current === "members" ? <MembersTab /> : current === "teams" ? <TeamsTab /> : current === "roles" ? <RolesTab /> : <ServiceAccountsTab />}
      </div>
    </Section>
  )
}
