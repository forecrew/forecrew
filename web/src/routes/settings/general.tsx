import { useState } from "react"
import { api } from "@/api/client"
import { keys, useChange, useMe } from "@/api/queries"
import { Field } from "@/components/Field"
import { Input } from "@/components/ui/input"
import { PermButton, useCan } from "@/lib/permissions"
import { Section } from "./layout"

/** Name and slug of the organization; only an Owner changes them. */
export function GeneralPage() {
  const me = useMe().data
  const can = useCan()
  const account = me?.account
  const [name, setName] = useState<string>()
  const [slug, setSlug] = useState<string>()
  const [saved, setSaved] = useState(false)
  const save = useChange((v: { name: string; slug: string }) => api.updateOrganization(v), keys.me)
  if (!account) return null
  const edit = can("org.manage")
  const shownName = name ?? account.name
  const shownSlug = slug ?? account.slug

  return (
    <Section permission="org.read" title="General">
      <form
        className="max-w-md space-y-4"
        noValidate
        onSubmit={(e) => {
          e.preventDefault()
          setSaved(false)
          save.mutate({ name: shownName.trim(), slug: shownSlug.trim() }, { onSuccess: () => setSaved(true) })
        }}
      >
        <Field label="Name">{(p) => <Input {...p} value={shownName} readOnly={!edit} onChange={(e) => setName(e.target.value)} />}</Field>
        <Field label="URL slug" hint="Links such as /login?org=acme carry it. Changing it breaks the links you sent before.">
          {(p) => <Input {...p} className="font-mono" value={shownSlug} readOnly={!edit} onChange={(e) => setSlug(e.target.value)} />}
        </Field>
        <PermButton permission="org.manage" type="submit" disabled={save.isPending}>
          Save
        </PermButton>
        {save.isError ? (
          <p role="alert" className="text-sm text-destructive">
            {save.error.message}
          </p>
        ) : null}
        {saved ? (
          <p role="status" className="text-sm text-muted-foreground">
            Saved.
          </p>
        ) : null}
      </form>
    </Section>
  )
}
