## Engineering rules

1. **Keep it simple.** Build the smallest thing that meets the acceptance criteria. Standard library first; a new dependency needs a one-line reason in the PR. No speculative options, layers or plugins.
2. **Reuse before writing.** Before adding a function, type, package or file, search the repo (`rg`) for similar functionality. If it exists, use it or extend it. Never duplicate code; shared helpers live in one place, test fakes live in `internal/testkit`.
3. **Everything is tested.** Every exported function and every meaningful branch, error paths included. Go table-driven tests; `httptest` for HTTP; real Postgres for store tests; Vitest and Testing Library for the web UI; no network in unit tests. A bug fix starts with a failing regression test. Every package under `internal/` and the web UI under `web/src` keep at least 80% coverage (`make cover`; generated code, shadcn's `components/ui` and `cmd/*` excluded, so `main` functions stay thin).
4. **Contracts are fixed.** `internal/domain` and `docs/contracts/` change only when the task says so. If you need a change, stop and describe it in your report.
5. **One task, one branch, one PR.** Branch `task/<Tnn>-<slug>` from `main`. Keep non-test changes small and focused.
6. **Small atomic commits.** Each commit holds one self-contained piece of functionality with its tests, and builds and passes tests on its own. Commit messages are short and written like a person would write them: one imperative line, for example `Add lease reaper` or `Fix lease expiry check`, with no body unless one short line is really needed.
7. **PR descriptions are short and explain the change:** what it does and why, and how it was tested, in a few lines.
8. **Never mention AI** in commits, branch names, PR titles or descriptions: no mention of AI, agents, models or tools that wrote the code, and no co-author or "generated with" lines.
9. **Conventions.** Follow `docs/contracts/conventions.md` (errors, logging, config, IDs, context).
10. **Secrets** are never logged, never written to disk, never committed.
11. **Done means:** `make lint`, `make test` and `make cover` pass, docs updated, and a report with summary, files changed, how it was tested (commands and results), assumptions, deviations and open questions.
