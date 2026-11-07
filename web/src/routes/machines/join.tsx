import { useQuery } from "@tanstack/react-query"
import { CheckIcon, CircleAlertIcon, CopyIcon, LoaderCircleIcon } from "lucide-react"
import { useState } from "react"
import { api } from "@/api/client"
import { useJoinCodes, useProjects } from "@/api/queries"
import type { JoinCode, Provider } from "@/api/types"
import { DraftPlan, useDraft } from "@/components/config/draft"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { gigabytes, humanName } from "@/lib/format"
import { RegisterImage } from "./register-image"

type Os = "linux" | "macos" | "windows"
const OS_NAMES: Record<Os, string> = { linux: "Linux", macos: "macOS", windows: "Windows" }
const RAW = "https://raw.githubusercontent.com/forecrew/forecrew/main/deploy/fleet-agent"

/** The browser's own system first. */
const detectOs = (): Os => (/Win/i.test(navigator.userAgent) ? "windows" : /Mac/i.test(navigator.userAgent) ? "macos" : "linux")

/** The line to run. The code is typed when asked: on a command line it would stay in the shell history. */
export function commandFor(os: Os, server: string): string {
  if (os === "windows") {
    return `$env:FORECREW_JOIN_CODE = Read-Host 'Join code'\niwr ${RAW}/install.ps1 -OutFile install.ps1\n.\\install.ps1 -Server ${server}`
  }
  return `curl -fsSLO ${RAW}/install.sh\nread -r FORECREW_JOIN_CODE && export FORECREW_JOIN_CODE\nsudo --preserve-env=FORECREW_JOIN_CODE sh install.sh --server ${server}`
}

function CopyBox({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <div className="flex items-start gap-2">
      <pre tabIndex={0} aria-label={label} className="min-w-0 flex-1 overflow-x-auto rounded-lg bg-muted p-3 font-mono text-xs">
        {text}
      </pre>
      <Button
        variant="outline"
        onClick={() => {
          void navigator.clipboard?.writeText(text).then(() => setCopied(true), () => setCopied(false))
        }}
      >
        {copied ? <CheckIcon aria-hidden="true" /> : <CopyIcon aria-hidden="true" />}
        {copied ? "Copied" : `Copy ${label.toLowerCase()}`}
      </Button>
    </div>
  )
}

/** What the machine said, as facts for the checks list: all of it is plain text it reported. */
function Checks({ code }: { code?: JoinCode }) {
  const f = code?.facts
  const claimed = code?.status === "claimed" || code?.status === "confirmed" || code?.status === "used"
  return (
    <section aria-labelledby="checks" className="space-y-1">
      <h3 id="checks" className="font-medium">
        Checks
      </h3>
      <ul aria-live="polite" className="space-y-1 text-sm">
        {claimed ? (
          <>
            <li className="flex items-center gap-2">
              <CheckIcon className="size-4 text-success" aria-hidden="true" /> Connected{f?.host ? ` from ${f.host}` : ""}
            </li>
            <li className="flex items-center gap-2">
              <CheckIcon className="size-4 text-success" aria-hidden="true" />{" "}
              {[(f?.os ?? []).map((o) => OS_NAMES[o as Os] ?? o).join(", "), f?.max_cpu ? `${f.max_cpu} CPU` : "", f?.max_memory_mb ? gigabytes(f.max_memory_mb) : ""].filter(Boolean).join(", ")}
            </li>
            {(f?.checks ?? []).map((c) => (
              <li key={c.name} className="flex items-center gap-2">
                {c.status === "ok" ? <CheckIcon className="size-4 text-success" aria-hidden="true" /> : <CircleAlertIcon className="size-4 text-warning" aria-hidden="true" />}
                <span>
                  {c.name}
                  {c.detail ? `: ${c.detail}` : ""}
                  {c.status === "ok" ? "" : c.status === "warn" ? " (check this)" : " (failed)"}
                </span>
              </li>
            ))}
          </>
        ) : (
          <li className="flex items-center gap-2 text-muted-foreground">
            <LoaderCircleIcon className="size-4" aria-hidden="true" /> {code ? "Waiting for the machine…" : "Waiting for the command to run…"}
          </li>
        )}
      </ul>
    </section>
  )
}

/** Small, Medium, and Large from 32 GB of memory up; the limit uses at most 75% of the host. */
export function proposeTypes(f: JoinCode["facts"]) {
  const cpu = f.max_cpu ?? 4
  const mem = f.max_memory_mb ?? 8192
  const sizes = [
    { label: "small", cpu: 2, memory_mb: 4096 },
    { label: "medium", cpu: 4, memory_mb: 8192 },
    ...(mem >= 32768 ? [{ label: "large", cpu: 8, memory_mb: 16384 }] : []),
  ].filter((s) => s.cpu <= cpu && s.memory_mb <= mem)
  const fit = Math.floor(Math.min((0.75 * cpu) / 4, (0.75 * mem) / 8192))
  let limit = Math.max(1, fit)
  if (f.max_machines) limit = Math.min(limit, f.max_machines)
  return { sizes: sizes.length ? sizes : [{ label: "small", cpu: 2, memory_mb: 4096 }], limit }
}

const kindOf = (code: JoinCode) => code.kind ?? "microvm"
const osOf = (code: JoinCode) => (code.facts.os?.[0] ?? "linux") as Os
const SAFE = (s: string) => s.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 40) || "location"

/** The whole join flow: a code, the command, the checks, the confirmation, and the plan of machine types. */
export function JoinFlow({ cloud, limitText, budget }: { cloud?: boolean; limitText?: string; budget?: number }) {
  const [os, setOs] = useState<Os>(detectOs())
  const [created, setCreated] = useState<{ id: string; code: string }>()
  const [error, setError] = useState("")
  const [name, setName] = useState("")
  const [provider, setProvider] = useState<Provider>()
  const draft = useDraft()
  const codes = useJoinCodes(Boolean(created))
  const code = codes.data?.find((c) => c.id === created?.id)
  const projects = useProjects()
  const [test, setTest] = useState("")

  const claimed = code?.status === "claimed"
  const suggested = code?.facts.host ? SAFE(code.facts.host) : ""
  const chosen = name || suggested
  const images = useQuery({ queryKey: ["images", code ? osOf(code) : ""], queryFn: () => api.listImages({ os: osOf(code!) }), enabled: Boolean(provider && code) })

  const start = async () => {
    setError("")
    try {
      const c = await api.createJoinCode(cloud)
      setCreated({ id: c.join_code.id, code: c.code })
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not make a join code")
    }
  }
  const confirm = async () => {
    if (!code?.facts_hash) return
    setError("")
    try {
      setProvider(await api.confirmJoin(code.id, chosen, code.facts_hash))
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not confirm it")
    }
  }
  const propose = async () => {
    if (!provider || !code) return
    const { sizes, limit } = proposeTypes(code.facts)
    const os = osOf(code)
    const image = images.data?.[0]?.name ?? os
    const arch = code.facts.arch?.[0] ?? "amd64"
    await draft.make(
      `Add machine types to ${humanName(provider.name)}`,
      [
        ...sizes.map((s) => ({
          kind: "shape",
          name: `${provider.name}-${s.label}`,
          spec: { provider: provider.name, os, arch, cpu: s.cpu, memory_mb: s.memory_mb, disk_gb: 20, max: limit, image, enabled: true },
        })),
        { kind: "provider", name: provider.name, spec: { kind: kindOf(code), mode: "agent", limits: { max_machines: limit, ...(budget ? { monthly_budget: budget } : {}) } } },
      ],
      `Add ${chosen}`,
    )
  }
  const testJob = async () => {
    const project = projects.data?.[0]?.name
    if (!project) {
      setTest("Onboard a repository first: a test job runs in a project.")
      return
    }
    try {
      await api.dispatchJob({ project, role: "reviewer", prompt: "This is a test job from Forecrew. Reply with the word ready and stop." })
      setTest(`Started a test job in ${project}. Watch it on Jobs.`)
    } catch (err) {
      setTest(err instanceof Error ? err.message : "Could not start a test job")
    }
  }
  const server = window.location.origin
  const windows = Boolean(code && (osOf(code) === "windows" || kindOf(code) === "hyperv"))

  return (
    <div className="max-w-3xl space-y-4">
      {!created ? (
        <>
          {limitText ? <p className="text-sm">{limitText}</p> : null}
          <Button onClick={() => void start()}>{cloud ? "Get the join code" : "Get the command"}</Button>
        </>
      ) : (
        <>
          <section aria-labelledby="run" className="space-y-2">
            <h2 id="run" className="text-lg font-semibold">
              {cloud ? "Give this to your cloud team" : "Run this on the machine"}
            </h2>
            {cloud ? (
              <p className="text-sm">The join code lasts 7 days. They run the kit in the AWS account, which only creates tagged resources in the subnets they choose, and paste the code into its one parameter.</p>
            ) : (
              <Tabs value={os} onValueChange={(v) => setOs(v as Os)}>
                <TabsList aria-label="System">
                  {(Object.keys(OS_NAMES) as Os[]).map((o) => (
                    <TabsTrigger key={o} value={o}>
                      {OS_NAMES[o]}
                    </TabsTrigger>
                  ))}
                </TabsList>
              </Tabs>
            )}
            {cloud ? null : <CopyBox label="Command" text={commandFor(os, server)} />}
            <CopyBox label="Join code" text={created.code} />
            <p className="text-sm text-muted-foreground">It reports what it finds. Nothing changes until you confirm. The code works once and does nothing alone.</p>
          </section>
          <Checks code={code} />
          {claimed && !provider ? (
            <section aria-labelledby="confirm" className="space-y-2 rounded-xl border p-4">
              <h2 id="confirm" className="text-lg font-semibold">
                Confirm this location
              </h2>
              <p className="text-sm">
                {kindOf(code) === "microvm" ? "A fresh Linux machine for each job, in about a second. No access to the host, the LAN or other machines." : kindOf(code) === "tart" ? "Fresh macOS machines, 2 at a time. That is Apple's limit. The first download is large, tens of GB." : kindOf(code) === "hyperv" ? "Fresh Windows machines. Forecrew cannot hand out Windows: you provide an image (below)." : kindOf(code) === "aws" ? "Machines start in your AWS account, and AWS bills you." : "Fresh machines for each job."}
              </p>
              <Field label="Name">{(p) => <Input {...p} className="font-mono" value={chosen} onChange={(e) => setName(e.target.value)} />}</Field>
              <Button onClick={() => void confirm()} disabled={!chosen}>
                Confirm
              </Button>
            </section>
          ) : null}
          {provider && !draft.plan ? (
            <section aria-labelledby="types" className="space-y-2 rounded-xl border p-4">
              <h2 id="types" className="text-lg font-semibold">
                {humanName(provider.name)} is added
              </h2>
              <p className="text-sm">Now choose what it can run. Machine types fit its hardware, and the limit uses at most 75% of the host.</p>
              <Button onClick={() => void propose()} disabled={draft.busy}>
                Suggest machine types
              </Button>
            </section>
          ) : null}
          {draft.error ? (
            <p role="alert" className="text-sm text-destructive">
              {draft.error}
            </p>
          ) : null}
          <DraftPlan draft={draft} onEdit={() => void draft.discard()} />
          {draft.plan?.status === "applied" ? (
            <section aria-labelledby="test" className="space-y-2">
              <h2 id="test" className="text-lg font-semibold">
                See it work
              </h2>
              <p className="text-sm">Run a test job, under a minute. Done is seeing a job run and the machine vanish.</p>
              <Button onClick={() => void testJob()}>Run a test job</Button>
              {test ? <p role="status">{test}</p> : null}
            </section>
          ) : null}
          {windows ? (
            <section aria-labelledby="image" className="space-y-2">
              <h2 id="image" className="text-lg font-semibold">
                Windows image
              </h2>
              <p className="text-sm">
                Forecrew cannot hand out Windows. When the installer asks, point it to a Windows ISO you are licensed for and it builds the image on the machine (about an hour; you can close this tab). Without one you get Run directly on this computer.
              </p>
              <RegisterImage />
            </section>
          ) : null}
        </>
      )}
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
    </div>
  )
}
