import { useState } from "react"
import { api } from "@/api/client"
import { keys, useChange } from "@/api/queries"
import type { CreatedToken, Scope } from "@/api/types"
import { Field } from "@/components/Field"
import { ShowOnce } from "@/components/ShowOnce"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"

const SCOPES: { value: Scope; label: string }[] = [
  { value: "read", label: "Read: look, change nothing" },
  { value: "write", label: "Write: also create goals, decide, cancel" },
  { value: "admin", label: "Admin: also manage the organization" },
]
const EXPIRIES = [
  { value: "168h", label: "7 days" },
  { value: "720h", label: "30 days" },
  { value: "2160h", label: "90 days" },
  { value: "8760h", label: "1 year" },
]

/**
 * Creates a personal token, or a service account's: a name, scopes and an
 * expiry. The secret is shown once, with Copy.
 */
export function CreateToken({ serviceAccount }: { serviceAccount?: string }) {
  const [name, setName] = useState("")
  const [scope, setScope] = useState<Scope>("read")
  const [ttl, setTtl] = useState("720h")
  const [error, setError] = useState("")
  const [created, setCreated] = useState<CreatedToken>()
  const create = useChange(
    (input: { name: string; scopes: Scope[]; ttl: string; service_account?: string }) => api.createToken(input),
    keys.tokens,
  )

  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(name)) {
      setError("Use lowercase letters, digits, dots, dashes and underscores, starting with a letter or digit.")
      return
    }
    setError("")
    create.mutate(
      { name, scopes: [scope], ttl, service_account: serviceAccount },
      {
        onSuccess: (token) => {
          setCreated(token)
          setName("")
        },
      },
    )
  }

  if (created) {
    return <ShowOnce title={`Token ${created.token.name}`} label="Token" secret={created.secret} onDismiss={() => setCreated(undefined)} />
  }
  return (
    <form onSubmit={submit} className="flex flex-wrap items-end gap-3" noValidate aria-label={serviceAccount ? `New token for ${serviceAccount}` : "New token"}>
      <Field label="Token name" error={error}>
        {(p) => <Input {...p} value={name} onChange={(e) => setName(e.target.value)} className="w-48 font-mono" />}
      </Field>
      <Field label="Scopes">
        {(p) => (
          <NativeSelect {...p} value={scope} onChange={(e) => setScope(e.target.value as Scope)}>
            {SCOPES.map((s) => (
              <NativeSelectOption key={s.value} value={s.value}>
                {s.label}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </Field>
      <Field label="Expires after">
        {(p) => (
          <NativeSelect {...p} value={ttl} onChange={(e) => setTtl(e.target.value)}>
            {EXPIRIES.map((x) => (
              <NativeSelectOption key={x.value} value={x.value}>
                {x.label}
              </NativeSelectOption>
            ))}
          </NativeSelect>
        )}
      </Field>
      <Button type="submit" disabled={create.isPending}>
        Create token
      </Button>
      {create.isError ? (
        <p role="alert" className="w-full text-sm text-destructive">
          {create.error.message}
        </p>
      ) : null}
    </form>
  )
}
