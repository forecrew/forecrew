/**
 * Well-known credential shapes (domain.SecretPatterns on the server). The
 * assistant box checks text before sending it, and the server refuses it too:
 * secrets go in a secure field.
 */
const SECRET = new RegExp(
  [
    "sk-ant-[A-Za-z0-9_-]{8,}",
    "sk-[A-Za-z0-9_-]{20,}",
    "gh[pousr]_[A-Za-z0-9]{20,}",
    "github_pat_[A-Za-z0-9_]{20,}",
    "fc[wmpb]_[A-Za-z0-9_-]{20,}",
    "A[KS]IA[A-Z0-9]{16}",
    "bearer\\s+[A-Za-z0-9._~+/=-]{16,}",
    "-----BEGIN [A-Z ]*PRIVATE KEY-----",
    "://[^/\\s:@]+:[^/\\s@]+@",
  ].join("|"),
  "i",
)

export const looksLikeSecret = (text: string) => SECRET.test(text)

/** Text that reads like something to build, which is a goal and not a setting. */
export const looksLikeGoal = (text: string) => /^\s*(build|implement|create|develop|add support|make it possible)\b/i.test(text)
