import { createContext, useContext } from "react"

/** What the app shell lets a page do with the assistant panel. */
export interface AssistantControl {
  /** Whether the caller may use it: it reads and plans configuration, so it needs config.read. */
  enabled: boolean
  open: boolean
  /** Opens the panel, with a message to start from. */
  ask: (message?: string) => void
  close: () => void
}

export const AssistantContext = createContext<AssistantControl>({ enabled: true, open: false, ask: () => {}, close: () => {} })
export const useAssistant = () => useContext(AssistantContext)
