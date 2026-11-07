/** An OAuth authorization request of this server: a page the web UI does not serve, so going there leaves the app. */
export function isAuthorization(target: string | undefined): target is string {
  return target !== undefined && target.startsWith("/oauth/authorize?")
}

/** The page to return to after login: a path on this site, else the home page. */
export function safeRedirect(target: string | undefined): string {
  // Browsers drop tabs and newlines from addresses and read a backslash as a
  // slash, so "/\t/evil.example" would leave the site: refuse them all.
  // eslint-disable-next-line no-control-regex
  if (target && target.startsWith("/") && !target.startsWith("//") && !/[\u0000-\u001f\u007f\\]/.test(target)) return target
  return "/"
}

/** Leaves the app for another address (the command line's port): a seam for tests. */
export const leave = {
  to: (url: string) => window.location.assign(url),
}
