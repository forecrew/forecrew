import "@testing-library/jest-dom/vitest"
import { cleanup } from "@testing-library/react"
import { afterAll, afterEach, beforeAll, beforeEach, vi } from "vitest"
import { server } from "./server"

// Node's fetch needs absolute URLs; the app calls relative ones.
const nodeFetch = globalThis.fetch
globalThis.fetch = (input, init) =>
  nodeFetch(typeof input === "string" && input.startsWith("/") ? new URL(input, window.location.origin) : input, init)

beforeAll(() => server.listen({ onUnhandledFrame: "error" }))
afterEach(() => {
  cleanup()
  server.resetHandlers()
})
afterAll(() => server.close())

// The fixtures were recorded at this time: ages like "45 min" depend on it.
beforeEach(() => {
  vi.useFakeTimers({ toFake: ["Date"] })
  vi.setSystemTime(new Date("2026-03-01T12:00:00Z"))
})
afterEach(() => vi.useRealTimers())

// jsdom lacks what the Radix and cmdk components use.
globalThis.ResizeObserver = class {
  observe() {}
  unobserve() {}
  disconnect() {}
}
Element.prototype.scrollIntoView = vi.fn()
window.scrollTo = vi.fn()
