/** The page to return to after login: a path on this site, else the home page. */
export function safeRedirect(target: string | undefined): string {
  if (target && target.startsWith("/") && !target.startsWith("//") && !target.startsWith("/\\")) return target
  return "/"
}
