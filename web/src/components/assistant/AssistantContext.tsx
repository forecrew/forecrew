import { createContext, useContext } from "react"

/** What the app shell lets a page do with the assistant panel. */
export interface AssistantControl {
  open: boolean
  /** Opens the panel, with a message to start from. */
  ask: (message?: string) => void
  close: () => void
  /** The text the panel starts with once, then forgets. */
  take: () => string | undefined
}

export const AssistantContext = createContext<AssistantControl>({ open: false, ask: () => {}, close: () => {}, take: () => undefined })
export const useAssistant = () => useContext(AssistantContext)
