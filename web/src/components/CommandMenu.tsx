import { useNavigate } from "@tanstack/react-router"
import { createContext, useContext, useEffect } from "react"
import { useGoals, useJobs, useProjects } from "@/api/queries"
import {
  Command, CommandDialog, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList,
} from "@/components/ui/command"
import { goalRef, jobRef } from "@/lib/format"

/** Lets a page open the command menu that the app shell owns. */
export const CommandMenuContext = createContext<() => void>(() => {})
export const useOpenCommandMenu = () => useContext(CommandMenuContext)

/** Opens the command menu from anywhere with Ctrl+K or Cmd+K. */
export function useCommandMenuShortcut(toggle: () => void) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault()
        toggle()
      }
    }
    document.addEventListener("keydown", onKey)
    return () => document.removeEventListener("keydown", onKey)
  }, [toggle])
}

const PAGES = [
  { label: "Home", to: "/" },
  { label: "Inbox", to: "/inbox" },
  { label: "Goals", to: "/goals" },
  { label: "Councils", to: "/councils" },
  { label: "Decisions", to: "/decisions" },
  { label: "Projects", to: "/projects" },
  { label: "Jobs", to: "/jobs" },
  { label: "Quality", to: "/quality" },
  { label: "Assumptions", to: "/assumptions" },
  { label: "Knowledge", to: "/knowledge" },
  { label: "Workers", to: "/workers" },
  { label: "Your account", to: "/account" },
  { label: "Settings", to: "/settings" },
] as const

function Results({ go }: { go: (to: string, params?: Record<string, string>) => void }) {
  const goals = useGoals()
  const projects = useProjects()
  const jobs = useJobs()
  return (
    <CommandList>
      <CommandEmpty>Nothing found.</CommandEmpty>
      <CommandGroup heading="Actions">
        <CommandItem value="new goal create" onSelect={() => go("/goals/new")}>
          New goal
        </CommandItem>
        <CommandItem value="onboard repository add project" onSelect={() => go("/projects/new")}>
          Onboard repository
        </CommandItem>
        <CommandItem value="start council debate decision question" onSelect={() => go("/councils/new")}>
          Start a council
        </CommandItem>
      </CommandGroup>
      <CommandGroup heading="Pages">
        {PAGES.map((p) => (
          <CommandItem key={p.to} value={`page ${p.label}`} onSelect={() => go(p.to)}>
            {p.label}
          </CommandItem>
        ))}
      </CommandGroup>
      {goals.data?.length ? (
        <CommandGroup heading="Goals">
          {goals.data.map((g) => (
            <CommandItem key={g.id} value={`${goalRef(g.number)} ${g.title}`} onSelect={() => go("/goals/$ref", { ref: goalRef(g.number) })}>
              <span className="font-mono">{goalRef(g.number)}</span> {g.title}
            </CommandItem>
          ))}
        </CommandGroup>
      ) : null}
      {projects.data?.length ? (
        <CommandGroup heading="Projects">
          {projects.data.map((p) => (
            <CommandItem key={p.id} value={`project ${p.name} ${p.repo}`} onSelect={() => go("/projects/$name", { name: p.name })}>
              {p.name} <span className="font-mono text-muted-foreground">{p.repo}</span>
            </CommandItem>
          ))}
        </CommandGroup>
      ) : null}
      {jobs.data?.length ? (
        <CommandGroup heading="Jobs">
          {jobs.data.map((j) => (
            <CommandItem key={j.id} value={`${jobRef(j.number)} ${j.role}`} onSelect={() => go("/jobs/$ref", { ref: jobRef(j.number) })}>
              <span className="font-mono">{jobRef(j.number)}</span> {j.role}
            </CommandItem>
          ))}
        </CommandGroup>
      ) : null}
    </CommandList>
  )
}

/** Jump to any page, goal, project or job, or start a goal or an onboarding. */
export function CommandMenu({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const navigate = useNavigate()
  const go = (to: string, params?: Record<string, string>) => {
    onOpenChange(false)
    void navigate({ to, params } as never)
  }
  return (
    <CommandDialog open={open} onOpenChange={onOpenChange} title="Command menu" description="Jump to a page, goal, project or job">
      <Command>
        <CommandInput placeholder="Search pages, goals, projects, jobs…" />
        <Results go={go} />
      </Command>
    </CommandDialog>
  )
}
