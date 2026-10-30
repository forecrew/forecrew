import { queryOptions, useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { api } from "./client"
import type { GateInfo } from "./types"

/** Lists refresh every 5 s. TanStack Query pauses polling while the tab is hidden. */
export const POLL_MS = 5000
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
