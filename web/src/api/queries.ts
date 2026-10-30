import { keepPreviousData, queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "./client"
import type { GateInfo, GoalStatus, JobEvent, JobStatus } from "./types"

/** Lists refresh every 5 s. TanStack Query pauses polling while the tab is hidden. */
export const POLL_MS = 5000
/** A running job's log refreshes every 2 s. */
export const LOG_POLL_MS = 2000

export const keys = {
  me: ["me"] as const,
  gates: ["gates"] as const,
  projects: ["projects"] as const,
  project: (name: string) => ["project", name] as const,
  goals: (project?: string, status?: string) => ["goals", project ?? "", status ?? ""] as const,
  goal: (ref: string) => ["goal", ref] as const,
  jobs: (project?: string, status?: string) => ["jobs", project ?? "", status ?? ""] as const,
  job: (ref: string) => ["job", ref] as const,
  logs: (ref: string) => ["job-logs", ref] as const,
  workers: ["workers"] as const,
}

const poll = { refetchInterval: POLL_MS, refetchIntervalInBackground: false }

export const meOptions = queryOptions({ queryKey: keys.me, queryFn: api.me, retry: false, staleTime: 60_000 })
export const useMe = () => useQuery(meOptions)
export const useGates = () => useQuery({ queryKey: keys.gates, queryFn: api.listGates, ...poll })
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
export const useGoal = (ref: string) => useQuery({ queryKey: keys.goal(ref), queryFn: () => api.getGoal(ref), ...poll })
export const useJobs = (project?: string, status?: JobStatus | "") =>
  useQuery({
    queryKey: keys.jobs(project, status),
    queryFn: () => api.listJobs({ project, status }),
    placeholderData: keepPreviousData,
    ...poll,
  })
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
      qc.setQueryData<GateInfo[]>(keys.gates, (old) => old?.filter((g) => g.ref !== v.ref))
    },
    onSettled: () => Promise.all([qc.invalidateQueries({ queryKey: keys.gates }), qc.invalidateQueries({ queryKey: ["goal"] })]),
  })
}
