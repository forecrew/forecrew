# Knowledge base

How forecrewd indexes, searches and guards a project's knowledge. The
contract (providers, policy, entry format, proposals) is
[contracts/knowledge.md](contracts/knowledge.md); this page describes what
runs today (providers, index and search). Access control and the proposal
flow are T37, curation and promotion of notes T39, external providers 4.19.

## What is indexed

One row per entry in `knowledge_entries`, from three sources:

| Kind | Provider | Section | Comes from |
|------|----------|---------|------------|
| `entry` | `repo` | the policy's sections | Markdown files of the repo under a section's paths |
| `generated` | `repo` | `generated` | the analyzer's facts, rebuilt on every push |
| `note` | `internal` | `notes` | what agents write with `notes_write` |

### Repo entries

The sections come from `.forecrew/knowledge-policy.yaml` at the default
branch head. Without that file the mapping is the repo's own layout: ADR
folders (`docs/adr`, `docs/decisions`, `doc/architecture/decisions` or the
folder `.adr-dir` names) are `architecture`, `CONTRIBUTING.md` is
`conventions`, CODEOWNERS is `owners`, `knowledge/vision/**` and
`knowledge/security/**` are protected, human-owned sections, and every other
folder `knowledge/<section>/` is a section of that name. The analyzer finds
the sources (`analyzer.KnowledgeSourcesOf`), the same ones it reports in the
repo facts. A folder that is not a valid section name, a reserved name
(`generated`, `notes`) or a file straight in `knowledge/` is no entry.

A glob with a wildcard indexes `.md`, `.mdx`, `.markdown` and `.txt` files up
to 256 KiB; a path that names one file (`CONTRIBUTING.md`, `CODEOWNERS`)
indexes it whatever it is. A file with frontmatter keeps its provenance; one
without gets it from git: `human` or, for a bot's commit, `agent`, the
commit as source and its time as creation. A file whose frontmatter breaks
its schema is skipped and reported in the sync result, and its earlier
version stays indexed.

### Sync

`Syncer.Sync` runs on a push to the default branch (the `push` webhook, which
the GitHub App must subscribe to) and in the reconciliation poller (every
five minutes, onboarded projects, a no-op while the branch head is the one
already synced). It lists the repo tree at the head, compares the blob SHAs
with the index, reads and indexes the entries that changed and deletes the ones
whose file is gone or no longer mapped. The index therefore always matches
the head, whichever events were missed. After `onboarding.complete` the
`knowledge.seed` action runs the same sync.

### Drift

On a push, `before..after` is compared. The push changed these:

- paths: every file that was modified, removed or renamed (the old and the
  new path);
- symbols: the exported Go identifiers (`func`, methods, `type`, `const`,
  `var`) and TypeScript exports (`function`, `class`, `const`, `let`, `var`,
  `interface`, `type`, `enum`) on changed lines of `.go`, `.ts` and `.tsx`
  files, at least five characters long.

An entry whose body mentions one of the paths (as a whole path) or one of the
symbols (as a whole word) gets `needs_reverify` with the reason
`commit <sha> changed <paths and symbols>`. Entries read in the same sync are
current and not flagged. Editing the entry (a new blob) clears the flag. The
poller does not know the previous commit, so a push the webhook missed
refreshes the index but flags nothing. T39's curator works through the flagged
entries.

### Generated section

On every new head forecrewd downloads the repo tarball with the App's token
into a temporary folder (no symlinks, size limits), runs the analyzer there and
removes the folder. It writes one entry per component (path, stack, manifests,
build, test), one `packages` entry (Go package folders and npm package
folders) and one `dependencies` entry (direct requirements of `go.mod`, not
`// indirect`, and `dependencies` and `devDependencies` of `package.json`).
These entries are read-only: never proposed to, never written to the repo, and
marked `generated` in results.

### Agent notes

`notes_write` (job MCP gateway) creates a note of scope `goal` (jobs of the
job's goal) or `project`, with tags and the job and role as provenance, or
replaces one of the job's own goal or project. Notes are indexed at once. At
most 50 calls per job attempt, 16 KiB per body, 10 tags. A goal note expires
seven days after its goal is done or canceled: a daily sweep stamps the expiry
(from the goal's last change) and deletes the notes past it. Humans delete a
note with `delete_note`.

## Search

Words search the title (weight A) and body (weight B) with Postgres full-text
search (`websearch_to_tsquery('english', ...)`, ranked by `ts_rank`). With an
embeddings key and pgvector the ranking mixes in similarity:

```
rank = 0.5 * ts_rank(search, query, 32) + 0.5 * cosine_similarity    (entries with an embedding)
rank = ts_rank(search, query, 32)                                    (the others)
```

An entry that the words do not match is still a hit when its cosine
similarity is at least 0.45. Embeddings are OpenAI `text-embedding-3-small`
at 1536 dimensions (it fits pgvector's index limit of 2000, is cheap and
needs only an OpenAI key); set `FORECREW_EMBEDDINGS_API_KEY` to turn them on.
They need pgvector: migration 00004 adds the column only where the extension
exists and 00005 gives it its dimension and a cosine index. Without the key or
the extension, or when the embeddings API fails, search is words only.

Tools: `kb_search` for agents (their role's `access.read` filters the sections;
the notes of other goals are never returned), `search_knowledge` and
`delete_note` for humans over MCP, REST (`GET /api/v1/knowledge`,
`DELETE /api/v1/knowledge/notes/{note}`) and the CLI:

```sh
forecrew knowledge search <project> <query> [--section] [--kind entry|note|generated] [--provider] [--limit]
forecrew knowledge delete-note <note-uuid>
```

Notes and every entry outside the repo provider's protected sections are
untrusted: their snippet reaches an agent (job MCP, human MCP) wrapped in
`<untrusted-data>` markers, and the `untrusted` flag is on every result.

## Seeding

After the profile is merged, `knowledge.seed` indexes the mapped paths and a
`knowledge-curator` job (strong tier, pool `build`, may write only
`knowledge/**`) proposes entries from `CLAUDE.md`, `AGENTS.md`, the README and
`docs/`, only for the seed sections no location covers, never inventing
conventions the sources do not state. Its PR `agent/kb-seed` waits at a
`knowledge_proposal` gate. A repo with ADRs gets no `architecture` entries.
Add `knowledge-curator: {runtime: claude, tier: strong}` under `roles` in
forecrew.yaml.

## Curated knowledge is not writable by roles

Every job of a writing role other than the curator gets its write paths
extended with `!knowledge/**`, `!.forecrew/knowledge-policy.yaml` and `!<glob>`
for each path of the project's sections, so curated knowledge changes only
through proposals (T37). The roles' packs list `!knowledge/**` themselves
where they write broadly.
