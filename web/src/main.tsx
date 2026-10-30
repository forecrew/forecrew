import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import "./index.css"

// The pages arrive with the commits that follow.
createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <p>Forecrew</p>
  </StrictMode>,
)
