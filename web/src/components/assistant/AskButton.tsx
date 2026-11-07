import { SparklesIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { useAssistant } from "./AssistantContext"

/** The small Ask button of every configuration page and empty state: opens the panel, which knows the page. Left out without config.read. */
export function AskButton({ message, label = "Ask" }: { message?: string; label?: string }) {
  const { ask, enabled } = useAssistant()
  if (!enabled) return null
  return (
    <Button variant="outline" onClick={() => ask(message)}>
      <SparklesIcon aria-hidden="true" /> {label}
    </Button>
  )
}
