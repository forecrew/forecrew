import { SparklesIcon } from "lucide-react"
import { Button } from "@/components/ui/button"
import { useAssistant } from "./AssistantContext"

/** The small Ask button of every configuration page and empty state: opens the panel, which knows the page. */
export function AskButton({ message, label = "Ask" }: { message?: string; label?: string }) {
  const { ask } = useAssistant()
  return (
    <Button variant="outline" onClick={() => ask(message)}>
      <SparklesIcon aria-hidden="true" /> {label}
    </Button>
  )
}
