import { useProjects } from "@/api/queries"
import { NativeSelect, NativeSelectOption } from "@/components/ui/native-select"

interface Props extends Omit<React.ComponentProps<"select">, "size" | "onChange"> {
  onChange: (v: string) => void
  /** Label of the empty choice, for a filter. Without it a project must be chosen. */
  allLabel?: string
}

/** A project picker, filled from the projects list. */
export function ProjectSelect({ onChange, allLabel, className, ...props }: Props) {
  const projects = useProjects()
  return (
    <NativeSelect className={className ?? "w-full sm:w-56"} {...props} onChange={(e) => onChange(e.target.value)}>
      <NativeSelectOption value="">{allLabel ?? "Choose a project"}</NativeSelectOption>
      {projects.data?.map((p) => (
        <NativeSelectOption key={p.id} value={p.name}>
          {p.name}
        </NativeSelectOption>
      ))}
    </NativeSelect>
  )
}
