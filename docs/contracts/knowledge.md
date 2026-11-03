# Knowledge base

Status: contract (T31). The index, the repo provider, notes and search are
T36; the policy, proposals and access requests T37; the learn step's
curation T39; external providers 4.19.

A project's knowledge base is an index in Postgres (`knowledge_entries`,
data-model.md) over **providers**: the project repo by default, later
Confluence, Notion, Google Drive or SharePoint. Entries live where the team
already keeps them; Forecrew indexes them, gives agents the relevant ones in
context packs and through the job MCP tool `kb_search` (job-mcp.md), and
routes changes through each provider's own review flow. Go: package
`internal/knowledge` (`ParsePolicy`, `ParseEntry`, `ProviderClient`).

## Sections

Every entry belongs to a section, a `domain.ValidName` such as
`architecture`, `conventions`, `testing`, `runbooks`, `security`, `lessons`,
`vision`, `glossary` or `market`. The policy maps each section to a
provider and paths. Two sections are reserved and kept by forecrewd:

- `generated`: components, packages and dependencies rebuilt from the
  analyzer on every push to the default branch. No provider, no paths, no
  owner, never protected, never proposed to and never written to the repo.
  `knowledge_entries.generated` is true exactly for this section.
- `notes`: agent notes, the only section of the built-in `internal`
  provider (below).

## Policy: .forecrew/knowledge-policy.yaml

Schema: [knowledge-policy.schema.json](knowledge-policy.schema.json);
`knowledge.ParsePolicy` checks it and applies the defaults. forecrewd reads
it at the default branch head. **The policy file itself is always
protected**: a change to it needs a human's approval, whatever it says, and
agents never grant themselves access (they call `request_access`).

```yaml
version: 1
providers:
  wiki:
    kind: confluence
    instance: acme.atlassian.net
    config: {space: ENG}
    credentials: acme-confluence     # a credential set configured on forecrewd
sections:
  architecture:
    paths: ["docs/adr/**", "docs/architecture/**"]   # provider repo by default
    owner: architect
  conventions:
    paths: [CONTRIBUTING.md]
    owner: codeowners
  runbooks:
    provider: wiki
    paths: ["space:ENG/page:123456"]
    owner: developer
    access: {propose: [developer, devops]}
  vision:
    owner: human
    protected: true
    access: {propose: [product-owner]}
  generated:
    access: {read: ["*"]}
```

| Key | Default | Meaning |
|-----|---------|---------|
| `version` | required | `1`. |
| `providers.<name>` | `repo: {kind: repo}` is always there | A provider. `<name>` is a `domain.ValidName`; `internal` is reserved. |
| `providers.<name>.kind` | required | `repo`, `confluence`, `notion`, `gdrive` or `sharepoint`. `internal` cannot be declared. |
| `providers.<name>.instance` | required except for `repo` | Host or organization, e.g. `acme.atlassian.net`. No credentials or user info. |
| `providers.<name>.config` | none | Settings without secrets, string, number or boolean values. Keys that look like secrets (`domain.SecretLikeName`) are refused. |
| `providers.<name>.credentials` | none | Name of a credential set an admin configured on forecrewd for this kind and instance (4.19). Never a secret, never an environment variable name: a repo cannot point forecrewd's secrets at a host of its choosing. |
| `sections.<name>.provider` | `repo` | A declared provider. |
| `sections.<name>.paths` | `knowledge/<name>/**` for a repo provider; required otherwise | Doublestar globs relative to the repo root (no `..`, no leading `/`) for a repo provider; provider locations (space, page or folder ids) otherwise. |
| `sections.<name>.owner` | `human` | Who approves proposals: a role, `human`, or `codeowners` (the CODEOWNERS reviewers of the changed paths, through the PR). |
| `sections.<name>.protected` | `false` | Changes only with a human's approval, whatever the owner. A protected section has no `write` access. |
| `sections.<name>.access.read` | `["*"]` | Roles that see the section in context packs and `kb_search`. `"*"` is every role. |
| `sections.<name>.access.propose` | `["*"]` | Roles that may call `kb_propose` on it. |
| `sections.<name>.access.write` | `[]` | Roles that may change it without a proposal (their `write_paths` still apply). |
| `sections.generated`, `sections.notes` | read by every role | Only `access.read` may be set. |

Without a policy file the default policy is T37's: the repo provider with
the profile's knowledge `integrations` mapping (T49) and
`knowledge/<section>/**` for the rest; `vision`, `security` and the policy
file protected and human-owned.

## Providers

A provider lists, fetches and reports changes of the entries under a
section's locations, and may search itself and accept proposals
(`internal/knowledge`):

```go
type ProviderClient interface {
	List(ctx context.Context, locations []string) ([]Ref, error)
	Fetch(ctx context.Context, path string) (Document, error) // ErrNotFound when gone
	Changes(ctx context.Context, locations []string, cursor string) (changes []Change, next string, err error)
}
type Searcher interface { // optional
	Search(ctx context.Context, locations []string, query string, limit int) ([]Ref, error)
}
type Proposer interface { // optional
	Propose(ctx context.Context, p domain.KnowledgeProposal) (externalRef string, err error)
}
```

- `Ref` is `{path, version}`: the repo path and blob SHA, or the provider's
  stable id and page version. `(project, provider, path)` is unique in the
  index.
- `Changes` with an empty cursor returns everything; the cursor of the repo
  provider is a commit SHA of the default branch. The sync loop indexes
  changed entries (by version) and removes deleted ones.
- `Propose` sends a proposal to the provider's review flow (the repo: a PR on
  `agent/kb-<job>` written by forecrewd; later a draft page or a comment)
  and returns the external ref it is followed by (PR number, page id). A
  provider without `Propose` gets a `knowledge_proposal` gate that shows the
  change for a human to copy across.
- Everything a provider returns is untrusted data. Only entries of protected
  repo sections, which humans approve, reach agents unmarked.

## Entries

An entry of the repo provider is a Markdown file with optional YAML
frontmatter, schema [knowledge-entry.schema.json](knowledge-entry.schema.json),
parsed by `knowledge.ParseEntry`:

```markdown
---
title: Queue jobs in Postgres
author_role: architect
source: JOB-41
created: 2026-10-01
last_verified: 2026-10-20T09:30:00Z
---
# ADR 1: Queue jobs in Postgres

We lease jobs with SELECT ... FOR UPDATE SKIP LOCKED.
```

| Key | Meaning |
|-----|---------|
| `title` | Default: the first `# ` heading, else the file name without extension. |
| `author_role` | Role that wrote it, or `human`. |
| `source` | A ref (`JOB-41`, `GOAL-3`, `KB-5`, `DEC-2`), a URL, or `human`. |
| `created` | `YYYY-MM-DD` or an RFC 3339 time; stored in UTC. |
| `last_verified` | When it was last checked against the code. |

Every key is optional and other tools' keys are ignored, so existing
documents index as they are. A file without `author_role`, `source` and
`created` gets its provenance from git (last commit and author kind).
Entries forecrewd writes always carry all of them. The index stores the
body without frontmatter, its SHA-256 (`body_hash`) and the provenance as
`domain.Provenance` (`{author_role, source, created, last_verified}`).

An entry is flagged `needs_reverify` with a reason (commit and paths) when a
push changes or deletes a path or symbol it mentions (T36); the curator
works through them.

## Agent notes: the internal provider

Besides the curated sections, agents share working knowledge in forecrewd's
own store: provider `internal`, section `notes`, rows of
`knowledge_entries` with `scope` (`goal` or `project`), `goal_id` (goal
notes) and `expires_at`, and `tags`.

- Written directly by agents with the job MCP tool `notes_write`: no
  proposal, no gate, no PR, indexed at once. A job writes and edits only
  notes of its own goal (scope `goal`) or project (scope `project`).
- Never protected, never owned, never proposed to.
- Goal notes are seen only by jobs of that goal and expire 7 days after the
  goal closes; a daily sweep deletes expired notes. Humans can read and
  delete notes (Knowledge page, `delete_note`).
- Notes are data, never instructions: always returned marked untrusted.
  The retro (T39) promotes the ones worth keeping into proposals for the
  curated sections.

## Search

`knowledge_entries.search` is a generated `tsvector` (title weight A, body
weight B, configuration `english`); queries use `websearch_to_tsquery(
'english', ...)` ranked by `ts_rank`, so search works on any Postgres. When
pgvector is installed (data-model.md, Rules) and an embedding key is set,
`embedding` similarity is mixed into the ranking (T36). Results carry their
kind (`entry`, `note` or `generated`), provider, section, path, source link,
provenance and `needs_reverify`, are filtered by the role's read access, and
never include notes of other goals.

## Proposals

A change to a curated section is a `knowledge_proposals` row, ref `KB-n`
(`domain.KnowledgeProposal`): section, path, action (`add`, `edit`,
`retire`), body, reason, proposer (role, or a human through
`propose_knowledge`), job and goal.

- **Batch:** a proposal made by a job of a goal to a section that is not
  protected is `per_goal`: it waits (`queued`) for the goal's learn PR.
  Protected sections and proposals outside a goal are `standalone`: sent to
  the provider when the job completes.
- **Status:** `queued` → `open` (at the provider, `external_ref` set, or at a
  `knowledge_proposal` gate) → `accepted` or `rejected`; a queued proposal
  can be rejected (policy, the learn step).
- **Approval** follows the section: protected → a `knowledge_proposal` gate
  to a human; owner `codeowners` → the PR's CODEOWNERS reviewers; owner a
  role → a review job of that role, then merge by policy; owner `human` → a
  human.
- Never to `generated` or `notes`, never from a role the policy does not
  allow to propose (dropped and audited).
