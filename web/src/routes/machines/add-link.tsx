import { Link } from "@tanstack/react-router"
import { buttonVariants } from "@/components/ui/button"

/** A link to the Add machines page, drawn as a button. Callers show it only to people who may manage machines. */
export function AddMachinesLink({ label = "Add machines" }: { label?: string }) {
  return (
    <Link to="/machines/add" className={buttonVariants()}>
      {label}
    </Link>
  )
}
