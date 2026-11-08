import { useQueryClient } from "@tanstack/react-query"
import { CheckIcon, CircleAlertIcon, CircleDashedIcon, LoaderCircleIcon } from "lucide-react"
import { useState } from "react"
import { api } from "@/api/client"
import { keys, useImageBuilds } from "@/api/queries"
import type { ImageBuild, Provider } from "@/api/types"
import { Field } from "@/components/Field"
import { StatusBadge } from "@/components/StatusBadge"
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Input } from "@/components/ui/input"
import { Progress, ProgressLabel, ProgressValue } from "@/components/ui/progress"
import { PermButton, useCan } from "@/lib/permissions"

/** The kinds of location whose machine can build a Windows image. */
export const buildsImages = (p: Pick<Provider, "kind" | "mode">) => (p.kind === "hyperv" || p.kind === "proxmox") && p.mode === "agent"

/**
 * The stages of a build, with the percent each starts at: they are the ones the connector's
 * progress.json reports (internal/fleetagent/imagebuild). The hour inside "Building" cannot be
 * measured, so the percent says where the build stands, not how long is left.
 */
export const STAGES = [
  { from: 0, label: "Checking this machine" },
  { from: 5, label: "Checking the ISO against its checksum" },
  { from: 10, label: "Fetching Packer's plugins" },
  { from: 15, label: "Building the image (about an hour)" },
  { from: 95, label: "Writing the manifest" },
]

type StageState = "done" | "current" | "failed" | "pending"

/** Where stage i stands in build b. */
export function stageState(i: number, b: Pick<ImageBuild, "state" | "percent">): StageState {
  if (b.state === "done") return "done"
  if (b.state === "requested") return "pending"
  const next = STAGES[i + 1]?.from ?? 101
  if (b.percent >= next) return "done"
  if (b.percent >= STAGES[i]!.from) return b.state === "failed" ? "failed" : "current"
  return "pending"
}

const STATE_BADGE = {
  requested: { tone: "attention", text: "Waiting for the connector" },
  running: { tone: "neutral", text: "Building" },
  done: { tone: "attention", text: "Built, not signed" },
  failed: { tone: "bad", text: "Failed" },
} as const

function StageList({ build }: { build: ImageBuild }) {
  return (
    <ol aria-label="Steps of the build" className="space-y-1 text-sm">
      {STAGES.map((s, i) => {
        const st = stageState(i, build)
        return (
          <li key={s.from} className="flex items-center gap-2" aria-current={st === "current" ? "step" : undefined}>
            {st === "done" ? <CheckIcon className="size-4 text-success" aria-hidden="true" /> : null}
            {st === "current" ? <LoaderCircleIcon className="size-4" aria-hidden="true" /> : null}
            {st === "failed" ? <CircleAlertIcon className="size-4 text-destructive" aria-hidden="true" /> : null}
            {st === "pending" ? <CircleDashedIcon className="size-4 text-muted-foreground" aria-hidden="true" /> : null}
            <span className={st === "pending" ? "text-muted-foreground" : undefined}>
              {s.label}
              {st === "done" ? " (done)" : st === "current" ? " (now)" : st === "failed" ? " (stopped here)" : ""}
            </span>
          </li>
        )
      })}
    </ol>
  )
}

/** What a running or waiting build looks like: the percent, the steps, and what the machine said. */
function Following({ build }: { build: ImageBuild }) {
  return (
    <div className="space-y-2 rounded-xl border p-3" role="status" aria-label="Image build">
      <p className="flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium">Windows image {build.version}</span>
        <StatusBadge tone={STATE_BADGE[build.state].tone}>{STATE_BADGE[build.state].text}</StatusBadge>
      </p>
      {build.state === "requested" ? (
        <p className="text-sm text-muted-foreground">
          Waiting for the connector on this machine to take the build. If it does not within 30 minutes, the build ends and says so.
        </p>
      ) : (
        <Progress value={build.percent}>
          <ProgressLabel>Progress</ProgressLabel>
          <ProgressValue />
        </Progress>
      )}
      <StageList build={build} />
      {build.step ? <p className="text-xs text-muted-foreground">The machine says: {build.step}</p> : null}
      <p className="text-xs text-muted-foreground">You can close this page. The build goes on, and this page shows it when you are back.</p>
    </div>
  )
}

const focusRegister = () => {
  const box = document.querySelector<HTMLTextAreaElement>('form[aria-label="Register an image"] textarea')
  box?.scrollIntoView({ block: "center" })
  box?.focus()
}

/** A build that ended: nothing is registered, and the way on is the person's. */
function Finished({ build, manage }: { build: ImageBuild; manage: boolean }) {
  if (build.state === "failed") {
    return (
      <Alert variant="destructive">
        <CircleAlertIcon aria-hidden="true" />
        <AlertTitle>The last build failed</AlertTitle>
        <AlertDescription>
          <p>{build.error || "The machine gave no reason."}</p>
          <p className="mt-1">Nothing was registered. Fix the cause and start the build again below.</p>
        </AlertDescription>
      </Alert>
    )
  }
  return (
    <section aria-labelledby="built" className="space-y-2 rounded-xl border p-3">
      <h3 id="built" className="flex flex-wrap items-center gap-2 font-medium">
        Windows image {build.version} is built <StatusBadge tone="attention">Not signed, not registered</StatusBadge>
      </h3>
      <p className="text-sm">Machines cannot use it yet. Forecrew never signs or registers it for you: you check it, sign it, then register it.</p>
      <ol className="list-decimal space-y-1 pl-5 text-sm [overflow-wrap:anywhere]">
        <li>
          Check the manifest on the machine: <code className="font-mono text-xs break-all">{build.manifest}</code>, for the image <code className="font-mono text-xs break-all">{build.digest}</code>
        </li>
        <li>
          Sign it with your image key, on a computer that is not that machine: <code className="font-mono text-xs">forecrew image sign &lt;manifest&gt; --key image.key</code>, and put the public key in the machine&apos;s <code className="font-mono text-xs">image-keys.pub</code>.
        </li>
        <li>
          Register the signed manifest
          {manage ? (
            <>
              :{" "}
              <Button variant="link" className="h-auto p-0" onClick={focusRegister}>
                go to Register the image
              </Button>
              .
            </>
          ) : (
            <>
              , which needs <code className="font-mono text-xs">worker.manage</code>.
            </>
          )}
        </li>
      </ol>
    </section>
  )
}

interface Draft {
  iso: string
  checksum: string
  chocolatey: string
  version: string
  edition: string
  licence: boolean
}

const EMPTY: Draft = { iso: "", checksum: "", chocolatey: "", version: "", edition: "", licence: false }

/**
 * Builds the Windows image on this location's machine, from an ISO the owner is licensed for.
 * What is sent is references: the ISO's path on the machine and its checksum, never the file.
 * The licence statement is the person's own to tick. The result is an unsigned manifest, and the page
 * only points to the person's way on: it never registers or signs.
 */
export function BuildImage({ provider }: { provider: Provider }) {
  const can = useCan()
  const manage = can("worker.manage")
  const client = useQueryClient()
  const builds = useImageBuilds(provider.name)
  const [draft, setDraft] = useState<Draft>(EMPTY)
  const [error, setError] = useState("")
  const [busy, setBusy] = useState(false)
  const latest = builds.data?.[0]
  const open = latest?.state === "requested" || latest?.state === "running"
  const set = (k: keyof Draft) => (e: React.ChangeEvent<HTMLInputElement>) => setDraft({ ...draft, [k]: e.target.value })
  const blocked = provider.status !== "active" ? "This location is not active yet. Finish adding it first." : !provider.online ? "The connector on this machine is offline. Start the build when it is back." : ""
  const ready = Boolean(draft.iso.trim() && draft.checksum.trim() && draft.chocolatey.trim() && draft.version.trim() && draft.licence)

  const start = async (e: React.FormEvent) => {
    e.preventDefault()
    setError("")
    setBusy(true)
    try {
      await api.startImageBuild(provider.name, {
        version: draft.version.trim(),
        iso: draft.iso.trim(),
        iso_checksum: draft.checksum.trim(),
        chocolatey_sha256: draft.chocolatey.trim(),
        ...(draft.edition.trim() ? { windows_image_name: draft.edition.trim() } : {}),
        license_confirmed: draft.licence,
      })
      setDraft(EMPTY)
      await client.invalidateQueries({ queryKey: keys.imageBuilds(provider.name) })
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not start the build")
    } finally {
      setBusy(false)
    }
  }

  const form = (
    <form onSubmit={(e) => void start(e)} aria-label="Build the Windows image" className="max-w-xl space-y-3" noValidate>
      <Field label="Windows ISO on this machine" hint="The path on the machine, such as D:\isos\server-2022.iso. The file stays there: Forecrew never sees it, and the connector reads only from the folder you allowed in its settings.">
        {(p) => <Input {...p} className="font-mono" disabled={!manage} value={draft.iso} onChange={set("iso")} />}
      </Field>
      <Field label="ISO checksum (SHA-256)" hint="Starts with sha256: and comes from your licence portal. The connector checks the file against it again.">
        {(p) => <Input {...p} className="font-mono" disabled={!manage} value={draft.checksum} onChange={set("checksum")} />}
      </Field>
      <Field label="Chocolatey installer hash (SHA-256)" hint="The image installs its tools with Chocolatey. Read its install.ps1, then paste the hash of the file you read.">
        {(p) => <Input {...p} className="font-mono" disabled={!manage} value={draft.chocolatey} onChange={set("chocolatey")} />}
      </Field>
      <Field label="Image version" hint="For example 2026.11.1.">
        {(p) => <Input {...p} className="font-mono" disabled={!manage} value={draft.version} onChange={set("version")} />}
      </Field>
      <Collapsible>
        <CollapsibleTrigger render={<Button type="button" variant="link" className="h-auto p-0" />}>More options</CollapsibleTrigger>
        <CollapsibleContent className="mt-2">
          <Field label="Windows edition" hint="The edition inside the ISO. Left empty, the build uses Windows Server 2022 Standard.">
            {(p) => <Input {...p} disabled={!manage} value={draft.edition} onChange={set("edition")} />}
          </Field>
        </CollapsibleContent>
      </Collapsible>
      <label className="flex items-start gap-2 text-sm">
        <Checkbox checked={draft.licence} disabled={!manage} onCheckedChange={(v) => setDraft({ ...draft, licence: v === true })} aria-label="I hold a Windows Server licence" className="mt-0.5" />
        <span>I hold a Windows Server licence that covers building and running this image. Forecrew does not provide Windows.</span>
      </label>
      {blocked ? <p className="text-sm text-muted-foreground">{blocked}</p> : null}
      <PermButton permission="worker.manage" type="submit" disabled={!ready || busy || Boolean(blocked)}>
        Build the image
      </PermButton>
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
    </form>
  )

  return (
    <section aria-labelledby="build-image" className="max-w-xl space-y-3">
      <h2 id="build-image" className="text-lg font-semibold">
        Windows image
      </h2>
      <p className="text-sm">
        Forecrew cannot hand out Windows. The connector on this machine builds the image from a Windows Server ISO you are licensed for, in about an hour. The result is not usable until you sign and register it.
      </p>
      {builds.isError ? <p className="text-sm text-muted-foreground">Could not read the builds of this machine.</p> : null}
      {latest && open ? <Following build={latest} /> : null}
      {latest && !open ? <Finished build={latest} manage={manage} /> : null}
      {open ? null : latest?.state === "done" ? (
        <Collapsible>
          <CollapsibleTrigger render={<Button type="button" variant="link" className="h-auto p-0" />}>Build another version</CollapsibleTrigger>
          <CollapsibleContent className="mt-2">{form}</CollapsibleContent>
        </Collapsible>
      ) : (
        form
      )}
    </section>
  )
}
