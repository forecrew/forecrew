# Knowledge base

How forecrewd indexes, searches and guards a project's knowledge. The
contract (providers, policy, entry format, proposals) is
[contracts/knowledge.md](contracts/knowledge.md); this page describes what
runs today (providers, index, search, access control and proposals).
Curation and promotion of notes are T39, external providers 4.19.

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
folder `knowledge/<section>/` is a section of that name. `architecture`
(owner `architect`), `conventions` (`reviewer`), `testing` (`test-architect`),
`runbooks` (`developer`) and `lessons` (`knowledge-curator`) always exist there,
at `knowledge/<section>/**` when the repo has no location for them, so agents
can propose to them. Every role reads and proposes. The analyzer finds
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

The webhook handler and the poller only queue a sync (`knowledge.Scheduler`);
one worker runs them, so a slow repo never holds up webhooks. Requests for a
project that is waiting are coalesced into one sync from the oldest previous
commit to the newest head, so drift covers every push. A sync that fails is
retried after 30 seconds, then 1, 2, 4 minutes and so on up to 30 minutes, also
when the poller asks again meanwhile, so an oversized repo is not downloaded
every five minutes. One sync reads at most 200 files from GitHub and saves
entries in batches of 20: a rate limit keeps what was read, and a sync that
stopped at its limit goes on at once.

A file that cannot be an entry (not UTF-8 text, a NUL byte, a blank
frontmatter title, a frontmatter that breaks its schema) is skipped, with its
reason in the sync result and the log; it never fails the sync, and the entry
indexed before it stays. The repo archive that feeds the generated section
counts every entry of every type against its limits (100000 entries, 512 MiB of
files, 8 MiB per file kept, and the same for the unpacked stream).

### Policy

The policy (`.forecrew/knowledge-policy.yaml`, or the default mapping) decides
who reads which section and which paths writing roles may not change. It is
kept in memory per project and read again from the default branch when a sync
runs. After a restart nothing is in memory: the first search or job reads it
from GitHub. A repo without commits, or without the default branch, has no
policy file and nothing to protect: the default policy holds, so its project
can onboard. When the read fails otherwise (GitHub down, a policy file that does not
parse) everything that needs it refuses: `kb_search` and `search_knowledge`
answer `unavailable` and a writing role's job is not created. There is no
default policy to fall back to, because the default would allow what a policy
may protect.

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
exists and 00006 gives it its dimension and a cosine index. Without the key or
the extension, or when the embeddings API fails, search is words only.

**What leaves forecrewd.** With `FORECREW_EMBEDDINGS_API_KEY` set and pgvector
installed, the title and body of every knowledge entry and agent note (cut to
8000 bytes each) and every search query are sent to the embeddings provider,
OpenAI. Repo text, notes and queries go to a third party that way; leave the
key unset to keep them in your installation. Without pgvector nothing is sent,
since the vectors could not be stored. Requests stay under the API limits: an
input is under 8192 tokens whatever it holds (one token per byte is assumed)
and a request holds at most 100000 bytes in 64 inputs. Errors are logged with
the status only, never with what the provider answered, since it may quote the
text. A request the provider refuses loses nothing: entries are saved first and
embedded in groups of 16, and every sync embeds up to 100 entries that have no
embedding yet.

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

## Context packs

Agents never read the whole knowledge base. When a job is created, its prompt
gets a `# Context` block with the sections the role's pack lists in
`context:` ([roles contract](contracts/roles.md)), highest priority first:

| Section | Holds |
|---------|-------|
| `knowledge` | the entries and generated facts that best match the task (or, without one, the goal): a search for any of the first ten distinctive words of its title and text, six results |
| `decisions` | the decisions of the job's goal, then the paths and titles of the project's ADRs (entries of the `architecture` section) |
| `assumptions` | the open assumptions the task rests on, the goal's own included, high impact first; for a job with a goal only, those of the goal. For the `test-engineer`, only those to verify by test |
| `similar_tasks` | up to five merged tasks of the project that best match the task or goal in a full-text search (any of the first ten distinctive words of its title and text), with their PR link and how often the reviewer saw them and the last verdict |
| `notes` | the notes agents wrote for the job's goal (not the project's), newest first, in full |
| `related_files` | paths the task text names (words with a `/` or a file extension) and the areas the goal's triage found |
| `test_plan` | the repo's test matrix, strategy and environments. It is not part of the budget below: it is cut with the prompt when that is too large |

Council jobs get no section but `test_plan`: members read what their stage may
see with `council.read`.

Searches run as the job's role, so a role never gets an entry of a section its
`access.read` does not list. When the knowledge policy cannot be read, the
sections that need it say so and the job starts anyway.

**Budget.** The pack has a size in characters: the role's `context_budget`, or
the default of the tier `forecrew.yaml` gives the role (fast 6000, strong
16000, best 32000). Sections are filled in the role's order, item by item, the
most relevant first. An item that does not fit is cut, with a note, when at
least 200 characters are left, and dropped otherwise; a section without room
for its heading is left out. Headings and the wrapping of untrusted text
count. The prompt says so: a line under a section tells how many items were
left out, and a last line names the sections left out whole. The pack also
gives way to the rest of the prompt: it gets only what is left of the prompt's
byte budget (120,000 bytes) after the goal, spec, task and feedback, so a large
goal is never cut for the pack.

**Untrusted text.** Decisions, assumptions, tasks, notes, triage areas and every
entry outside the repo provider's protected sections are written by agents or
repos: each reaches the prompt between `<untrusted-data>` markers, and the
prompt says so. Only refs, dates, validated paths and PR links stand outside.

**Record.** The job stores a manifest, `context_manifest` in `job_status` and
`GET /api/v1/jobs/{job}` (`forecrew job show` summarizes it): the budget, the
size and, for each listed section, its size, the `items` that went in (`id` or
`path`, `size`) and the `dropped` ones with the size they would have had. A
role without a `context:` list has none.

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

Every job of a writing role gets its write paths extended with
`!.forecrew/knowledge-policy.yaml`, `!knowledge/**` and `!<glob>` for each path
of the project's repo sections, so curated knowledge changes only through
proposals (T37). The knowledge curator, who writes the fallback `knowledge/`
folder, keeps `knowledge/**` but gets the policy file and the globs of the
protected sections excluded. A role that holds `access.write` on a section
changes its files without a proposal: the section's globs are added to its
paths instead of excluded, and a grant under `knowledge/` also takes
`!knowledge/**` away (an exclusion beats an include), so the files there that
no section maps are then writable by that role. The roles' packs list
`!knowledge/**` themselves where they write broadly. Exclusions ignore case, as
the protection of the sections does: `Knowledge/Vision/x.md` is
`knowledge/vision/x.md` on a case-insensitive checkout.

## Proposals

Agents never change curated knowledge. They propose, section owners approve
in the tool where the section lives, and `internal/knowledge/proposals` runs
the flow. Agent notes are outside it.

A role proposes with the job MCP tool `kb_propose` ({section, path, action,
body, reason}). `Policy.Target` decides. It refuses, and the proposal is
dropped with an audit entry (`knowledge.proposal_dropped`), when:

- the section is `generated`, `notes` or unknown;
- the role is not in `access.propose` of the section, or of any other section
  whose globs match the path, so a path of a protected section cannot be
  proposed under another section's name;
- the path is the policy file, whatever the policy says about itself, lies
  under `generated/`, which forecrewd keeps, or is not an entry of the section.

Case does not count for these paths or for protected sections: on a
case-insensitive checkout `knowledge/Vision/` is `knowledge/vision/`, so a
section of `**` does not get around the protection with another spelling.

The check runs again when the proposal is sent, against the policy of that
moment. Proposal bodies are data: forecrewd writes the provenance frontmatter
above the body, so a body cannot forge it, and the PR text marks reason and
text as data.

**When it goes out.** A proposal of a goal's job to a section that is not
protected is `per_goal`: it stays `queued` for the goal's learn PR (T39). The
others are `standalone`. In the transaction that finishes a job successfully,
forecrewd stores one run of the playbook `knowledge-proposal` per provider
with such proposals, so a crash loses none. The standalone proposals of a job
that fails or is canceled are not sent: they are rejected in the transaction
that ends the job (`knowledge.proposal_rejected`). Its per_goal proposals stay
`queued` for the learn step.

**Review.** The repo provider's review flow is one PR `agent/kb-<job>`
written by forecrewd through the GitHub API, which needs no agent run. A
provider without one gets a `knowledge_proposal` gate that shows the change;
approving it says a human copied it across. One PR can carry several
sections, and the strictest owner wins:

| Owner of a section | What happens |
|--------------------|--------------|
| protected, or `human` | A `knowledge_proposal` gate on the PR; approving merges it. Resolving it needs `gate.approve.knowledge_protected` for a protected section, else `gate.approve.knowledge`. |
| a role | A read-only review job of that role on the branch, answering with the reviewer's verdict. `approve` merges it after the checks; any other verdict, or a review that cannot be read, opens the human gate. A role the project does not have counts as `human`. |
| `codeowners` | Nothing of Forecrew's own: GitHub requests the CODEOWNERS reviewers for the changed paths and a human merges. The branch rules must allow PRs from the App, and must not stop a reviewer from merging. |

`request_changes` and `reject` both reject the proposals and close the PR; the
feedback is in the audit log for a new proposal. A PR merged or closed on
GitHub at any point settles the proposals (`pr.merged`, `pr.closed`, the
webhook and the poller). forecrewd merges with `github.merge_pr`, so branch
rules that demand a human review make it escalate to a `needs_you` gate. It
merges only the head it sent for review: after a push to the branch the merge
escalates too, and a human closes the PR or merges it on GitHub.

**Humans** use `propose_knowledge`: straight to a PR `agent/kb-KB-n`, no job
and no gate, merged on GitHub like any PR. For a protected section the caller
needs `gate.approve.knowledge_protected` and is recorded as approver
(`knowledge.proposal_approved` and a PR comment).

## Access requests

A role that needs more calls `request_access` ({section, level, reason}) and
goes on without it. forecrewd opens an `access_request` gate and, in the same
transaction, a run of the playbook `knowledge-access`. Approving opens a PR
`agent/kb-access-GATE-n` on the policy file, which a human merges like any
PR; rejecting changes nothing. A repo without a policy file gets one that
spells out the whole default policy, because a file replaces the defaults.
