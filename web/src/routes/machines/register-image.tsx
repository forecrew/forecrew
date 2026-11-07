import { useState } from "react"
import { api } from "@/api/client"
import type { ImageManifest } from "@/api/types"
import { Field } from "@/components/Field"
import { Button } from "@/components/ui/button"
import { Textarea } from "@/components/ui/textarea"

/**
 * Registers a built, signed image. Only a person does it, with the signed manifest the build
 * printed: the connector never registers images itself. The server checks the form and the
 * connector checks the signature.
 */
export function RegisterImage() {
  const [text, setText] = useState("")
  const [error, setError] = useState("")
  const [done, setDone] = useState("")
  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError("")
    setDone("")
    let manifest: ImageManifest
    try {
      manifest = JSON.parse(text) as ImageManifest
    } catch {
      setError("That is not JSON. Paste the signed manifest the image build printed.")
      return
    }
    try {
      const image = await api.registerImage(manifest)
      setDone(`Registered ${image.name} ${image.version}.`)
      setText("")
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not register it")
    }
  }
  return (
    <form onSubmit={(e) => void submit(e)} aria-label="Register an image" className="max-w-xl space-y-2" noValidate>
      <Field label="Windows image" hint="When the image is built, paste its signed manifest to make it available. Forecrew cannot hand out Windows itself.">
        {(p) => <Textarea {...p} rows={5} className="font-mono text-xs" value={text} onChange={(e) => setText(e.target.value)} />}
      </Field>
      <Button type="submit" disabled={!text.trim()}>
        Register the image
      </Button>
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      ) : null}
      {done ? <p role="status">{done}</p> : null}
    </form>
  )
}
