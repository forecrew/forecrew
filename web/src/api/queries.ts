import { keepPreviousData, queryOptions, useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query"
import { api } from "./client"
import type { AssumptionStatus, CouncilStatus, GateInfo, GoalStatus, JobEvent, JobStatus, StartCouncilInput } from "./types"

/** Lists refresh every 5 s. TanStack Query pauses polling while the tab is hidden. */
export const POLL_MS = 5000
/** A running job's log refreshes every 2 s. */
export const LOG_POLL_MS = 2000

export const keys = {
  me: ["me"] as const,
  gates: (project?: string) => ["gates", project ?? ""] as const,
  projects: ["projects"] as const,
  project: (name: string) => ["project", name] as const,
  goals: (project?: string, status?: string) => ["goals", project ?? "", status ?? ""] as const,
  goal: (ref: string) => ["goal", ref] as const,
  jobs: (project?: string, status?: string) => ["jobs", project ?? "", status ?? ""] as const,
  goalJobs: (goal: string) => ["goal-jobs", goal] as const,
  job: (ref: string) => ["job", ref] as const,
  logs: (ref: string) => ["job-logs", ref] as const,
  councils: (project?: string, status?: string) => ["councils", project ?? "", status ?? ""] as const,
  council: (ref: string) => ["council", ref] as const,
  decisions: (project?: string) => ["decisions", project ?? ""] as const,
  assumptions: (project: string) => ["assumptions", project] as const,
  artifacts: (job: string) => ["artifacts", job] as const,
  quality: (project: string) => ["quality", project] as const,
  verification: (change: string, project: string) => ["verification", change, project] as const,
  workers: ["workers"] as const,
  tokens: ["tokens"] as const,
}

const poll = { refetchInterval: POLL_MS, refetchIntervalInBackground: false }

export const meOptions = queryOptions({ queryKey: keys.me, queryFn: api.me, retry: false, staleTime: 60_000 })
export const useMe = () => useQuery(meOptions)
export const useGates = (project?: string) =>
  useQuery({
    queryKey: keys.gates(project),
    queryFn: () => api.listGates({ project }),
    placeholderData: keepPreviousData,
    ...poll,
  })
export const useProjects = () => useQuery({ queryKey: keys.projects, queryFn: api.listProjects, ...poll })
export const useProject = (name: string) =>
  useQuery({ queryKey: keys.project(name), queryFn: () => api.getProject(name), ...poll })
export const useGoals = (project?: string, status?: GoalStatus | "") =>
  useQuery({
    queryKey: keys.goals(project, status),
    queryFn: () => api.listGoals({ project, status }),
    placeholderData: keepPreviousData,
    ...poll,
  })
export const useGoal = (ref: string, enabled = true) =>
  useQuery({ queryKey: keys.goal(ref), queryFn: () => api.getGoal(ref), enabled, ...poll })
export const useJobs = (project?: string, status?: JobStatus | "") =>
  useQuery({
    queryKey: keys.jobs(project, status),
    queryFn: () => api.listJobs({ project, status }),
    placeholderData: keepPreviousData,
    ...poll,
  })
export const useGoalJobs = (goal: string) =>
  useQuery({ queryKey: keys.goalJobs(goal), queryFn: () => api.listJobs({ goal, limit: 500 }), ...poll })
export const useJob = (ref: string) =>
  useQuery({
    queryKey: keys.job(ref),
    queryFn: () => api.getJob(ref),
    refetchInterval: (q) => (q.state.data && ["succeeded", "failed", "canceled"].includes(q.state.data.status) ? false : POLL_MS),
    refetchIntervalInBackground: false,
  })

/** Runs a per-project list call for one project, or for every project when none is chosen; newest first. */
async function acrossProjects<T extends { created_at: string }>(qc: QueryClient, project: string | undefined, fetch: (name: string) => Promise<T[]>) {
  const names = project ? [project] : (await qc.ensureQueryData({ queryKey: keys.projects, queryFn: api.listProjects })).map((p) => p.name)
  return (await Promise.all(names.map(fetch))).flat().sort((a, b) => b.created_at.localeCompare(a.created_at))
}

export const useCouncils = (project?: string, status?: CouncilStatus | "") => {
  const qc = useQueryClient()
  return useQuery({
    queryKey: keys.councils(project, status),
    queryFn: () => acrossProjects(qc, project, (name) => api.listCouncils({ project: name, status })),
    placeholderData: keepPreviousData,
    ...poll,
  })
}
export const useDecisions = (project?: string) => {
  const qc = useQueryClient()
  return useQuery({
    queryKey: keys.decisions(project),
    queryFn: () => acrossProjects(qc, project, (name) => api.listDecisions({ project: name })),
    placeholderData: keepPreviousData,
    ...poll,
  })
}
/** A council; it refreshes every 5 s until it ends. */
export const useCouncil = (ref: string, enabled = true) =>
  useQuery({
    queryKey: keys.council(ref),
    queryFn: () => api.getCouncil(ref),
    enabled,
    refetchInterval: (q) => (q.state.data && q.state.data.status !== "running" ? false : POLL_MS),
    refetchIntervalInBackground: false,
  })
export const useAssumptions = (project: string, enabled = true) =>
  useQuery({
    queryKey: keys.assumptions(project),
    queryFn: () => api.listAssumptions({ project }),
    enabled,
    ...poll,
  })
export const useArtifacts = (job: string) => useQuery({ queryKey: keys.artifacts(job), queryFn: () => api.listArtifacts(job) })
export const useQuality = (project: string) => useQuery({ queryKey: keys.quality(project), queryFn: () => api.getQuality(project), ...poll })
/** The verification report of a change; a 404 (no report yet) is an error the caller can read. */
export const useVerification = (change: string, project = "", enabled = true) =>
  useQuery({ queryKey: keys.verification(change, project), queryFn: () => api.getVerification(change, project), enabled, retry: false, ...poll })
export const useTokens = () => useQuery({ queryKey: keys.tokens, queryFn: api.listTokens, ...poll })
export const useWorkers = () => useQuery({ queryKey: keys.workers, queryFn: api.listWorkers, ...poll })

export interface LogState {
  events: JobEvent[]
  nextSeq: number
}

/**
 * The event log of a job. Each fetch asks only for the events after the last
 * one it holds (the cursor) and appends them; while the job runs it repeats
 * every 2 s.
 */
export function useJobLog(ref: string, running: boolean) {
  const qc = useQueryClient()
  return useQuery({
    queryKey: keys.logs(ref),
    queryFn: async (): Promise<LogState> => {
      const prev = qc.getQueryData<LogState>(keys.logs(ref)) ?? { events: [], nextSeq: 0 }
      const events = [...prev.events]
      let cursor = prev.nextSeq
      for (;;) {
        const page = await api.jobLogs(ref, cursor)
        events.push(...page.events)
        cursor = page.next_seq
        if (page.events.length < 500) break
      }
      return { events, nextSeq: cursor }
    },
    refetchInterval: running ? LOG_POLL_MS : false,
    refetchIntervalInBackground: false,
  })
}

/** Adds a constraint to a running council; the council refreshes. */
export function useAddConstraint(ref: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (constraint: string) => api.addConstraint(ref, constraint),
    onSettled: () => qc.invalidateQueries({ queryKey: ["council"] }),
  })
}

/** Starts a council; the council lists refresh. */
export function useStartCouncil() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (input: StartCouncilInput) => api.startCouncil(input),
    onSettled: () => qc.invalidateQueries({ queryKey: ["councils"] }),
  })
}

/** Verifies an assumption with evidence; its review gate closes with it. */
export function useVerifyAssumption() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { ref: string; evidence: string }) => api.resolveAssumption(v.ref, "verified" satisfies AssumptionStatus, v.evidence),
    onSuccess: (a) =>
      qc.setQueriesData<GateInfo[]>({ queryKey: ["gates"] }, (old) => old?.filter((g) => !(g.kind === "assumption_review" && g.title.startsWith(`Review ${a.ref}:`)))),
    onSettled: () => Promise.all(["gates", "assumptions", "goal"].map((k) => qc.invalidateQueries({ queryKey: [k] }))),
  })
}

/** Cancels a job; after it the job, the job lists and the gates (a needs_you gate may appear) refresh. */
export function useCancelJob() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (ref: string) => api.cancelJob(ref),
    onSettled: () =>
      Promise.all(["job", "jobs", "goal-jobs", "goal", "gates"].map((k) => qc.invalidateQueries({ queryKey: [k] }))),
  })
}

/** Revokes a token by ID; the token list refreshes, unless it was the token in use (the next call would be a 401). */
export function useRevokeToken() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.revokeToken(id),
    onSettled: (res) => (res?.self ? undefined : qc.invalidateQueries({ queryKey: keys.tokens })),
  })
}

/** Gate decisions: after one, the inbox and the goals refresh. */
export function useDecide() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { ref: string; decision: "approve" | "request-changes" | "reject"; feedback?: string }) => {
      switch (v.decision) {
        case "approve":
          return api.approve(v.ref, v.feedback)
        case "reject":
          return api.reject(v.ref, v.feedback)
        default:
          return api.requestChanges(v.ref, v.feedback ?? "")
      }
    },
    onSuccess: (_, v) => {
      qc.setQueriesData<GateInfo[]>({ queryKey: ["gates"] }, (old) => old?.filter((g) => g.ref !== v.ref))
    },
    onSettled: () =>
      Promise.all(["gates", "goal", "council", "councils", "decisions", "assumptions"].map((k) => qc.invalidateQueries({ queryKey: [k] }))),
  })
}
