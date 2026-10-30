import { keepPreviousData, queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "./client"
import type { GateInfo, GoalStatus, JobEvent, JobStatus } from "./types"

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
  workers: ["workers"] as const,
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

/** Cancels a job; after it the job, the job lists and the gates (a needs_you gate may appear) refresh. */
export function useCancelJob() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (ref: string) => api.cancelJob(ref),
    onSettled: () =>
      Promise.all(["job", "jobs", "goal-jobs", "goal", "gates"].map((k) => qc.invalidateQueries({ queryKey: [k] }))),
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
    onSettled: () => Promise.all([qc.invalidateQueries({ queryKey: ["gates"] }), qc.invalidateQueries({ queryKey: ["goal"] })]),
  })
}
