# Fleet agent API

The fleet agent API is how `forecrew-fleet-agent` talks to forecrewd. Like the
worker, the agent runs where the machines are and dials out over HTTPS;
forecrewd never connects to it, and the credentials of the place (a Proxmox
token, an AWS role) never leave it. Paths are under `/fleet/v1/`, bodies are
JSON, times are RFC 3339 in UTC, IDs are UUID strings. Bodies are
`$defs` of `fleet-agent-api.schema.json`. Implemented by T59.

The model is level-triggered: forecrewd says which machines should exist, the
agent makes it so and reports what happened. A lost message is repaired by the
next poll.

## Authentication

Every request but `join` carries `Authorization: Bearer <provider token>`.
forecrewd hashes the token (SHA-256), looks it up with
`CapacityProviders.GetByTokenHash` and answers `401` to an unknown token or a
provider with status `revoked`. The token identifies the provider and so its
organization. A token is `fcp_<43 base64url chars>`, stored only as a hash and
revocable. It comes from `forecrew provider add`, which shows it once, for
scripts and air-gapped sites, or from `join`, which shows it to nobody.

## Secrets

- A `bootstrap` in a poll response and the `token` of a confirmed join are the
  only secrets on this API. They travel only in these response bodies.
- Neither side logs a request or response body of this API, a bootstrap token, a
  provider token, a join code or a claim. Go types holding them hide them
  (`fleet.Bootstrap`), and `domain.SecretPatterns` matches `fcp_` and `fcb_`
  for the log scrubbers.
- The agent keeps them in memory only, except its own provider token, which it
  stores readable by its user alone, like a worker's token.
- Nothing the agent's local credentials allow is ever sent: capabilities hold
  facts, not secrets.

## Errors

The body is the worker API's: `{"error": {"code": "...", "message": "..."}}`.

| Status | `code` | When |
|--------|--------|------|
| 400 | `bad_request` | Malformed JSON, unknown field values, a state the agent may not report. |
| 401 | `unauthorized` | Missing, unknown or revoked token; on `join`, an unknown, used or expired code. |
| 404 | `not_found` | The machine is not this provider's. |
| 409 | `conflict` | A transition the machine cannot make; on `join`, a code claimed by another agent. |
| 413 | `too_large` | Body over 1 MiB. |
| 429 | `rate_limited` | Too many `join` calls from one address. |
| 500 | `internal` | Server error. |

The agent retries 5xx and network errors with exponential backoff (1 s up to
30 s). It stops on `401` and says so in its log; a revoked provider gets no
more demand.

## Offline

While forecrewd cannot be reached the agent keeps its machines running and
creates and destroys nothing new; deadlines it already knows still apply. After
reconnecting it registers, reconciles and polls. forecrewd shows the provider
offline after 60 s without contact, starts nothing there, and does not fail its
machines: their workers still heartbeat or their leases expire as usual.

## Joining

A join code connects an agent with no token shown to anyone. It is drawn by the
Connect step, is random, is stored as a hash (`provider_join_codes`), and lasts
`domain.JoinCodeTTL` (1 hour) or `domain.JoinCodeCloudTTL` (7 days, for a kit
another team runs). Alone it can do nothing: it gives no access to demand, and it
works once.

### POST /fleet/v1/join

No bearer token. The agent generates `claim`, 32 random bytes in base64url, and
keeps it in memory.

```json
{
  "code": "K7PQ-2M4X",
  "claim": "pXk...43 chars",
  "kind": "microvm",
  "facts": {"os": ["linux"], "arch": ["amd64"], "max_cpu": 16, "max_memory_mb": 65536,
            "image_formats": ["rootfs"], "checks": [{"name": "kvm", "status": "ok"}], "host": "nas"}
}
```

The first call binds the code to the claim's hash and stores `kind` and `facts`
for the plan a person confirms. Later calls must carry the same claim (`409`
otherwise), update the facts, and are how the agent waits:

```json
{"status": "waiting", "retry_after_seconds": 5}
```

When a person has confirmed, forecrewd has created the provider, and the next
call mints the token, stores its hash, marks the code used and answers once:

```json
{"status": "confirmed", "token": "fcp_..."}
```

A code past its expiry, or used, is `401`. An agent that loses the answer starts
over with a new code.

## POST /fleet/v1/register

Sent at start and after every reconnect, and whenever capabilities change. It
records the capabilities and the contact (`CapacityProviders.Seen`).

Request: `{"capabilities": {...}}`, `domain.ProviderCapabilities`.

Response `200`:

```json
{"id": "018f...", "name": "home-server", "kind": "microvm", "status": "active",
 "limits": {"max_machines": 3}, "tags": {"owner": "platform"}, "poll_timeout_seconds": 30}
```

## POST /fleet/v1/poll

Long-polls the machines that should exist here, and records the contact.

Request: `{"generation": 41}`, the last generation seen; absent or 0 on the
first poll.

forecrewd answers at once when the provider's desired machines changed since
`generation`, otherwise it waits up to 30 s and answers with the same list. The
agent polls again right away; its HTTP client timeout must be at least 45 s.

Response `200`:

```json
{
  "generation": 42,
  "machines": [
    {
      "machine_id": "018f...",
      "shape": {"...": "domain.Shape"},
      "image": {"...": "domain.Image, resolved"},
      "bootstrap": {"server_url": "https://forecrew.example.com", "token": "fcb_..."},
      "tags": {"forecrew:machine": "018f...", "forecrew:org": "acme"},
      "network_allow": ["10.0.0.5"],
      "deadline": "2026-03-01T13:00:00Z"
    }
  ]
}
```

`machines` is the whole desired set: every machine of this provider in
`requested`, `booting`, `ready` or `busy`. The agent:

- creates a machine it does not have, with `fleet.Provider.Create`;
- destroys a machine it has that is **not** in the list, and reports it;
- changes nothing for a machine it has that is in the list.

`bootstrap` is present only on machines still `requested`. forecrewd keeps only
the hash, so each poll that returns a `requested` machine mints a new token and
replaces the old one. That is why `Create` restarts a machine that exists: a
retry after a crash must boot with the token just given. `deadline` is absent
until the machine has a job. A `paused` provider gets no `requested` machines.

## POST /fleet/v1/machines/{machine_id}/state

Reports what happened to a machine. A report that repeats the current state is
accepted, so the agent can retry.

Request:

```json
{"state": "booting", "external_id": "vm-1842"}
```

| `state` | Meaning |
|---------|---------|
| `booting` | Created; `external_id` is required. From `requested`. |
| `destroyed` | Gone, as asked or found gone. Writes `destroyed_at`. |
| `failed` | It broke; `error` says why, at most 2000 characters, with no secrets. |

`ready`, `busy` and `draining` belong to forecrewd: a report of them is `400`.
`fleet.ErrCapacity` from `Create` is not reported; the agent tries again on the
next poll. A spot machine taken back is `failed`, and its job is requeued
through its lease.

Response `200`: `{}`.

## POST /fleet/v1/reconcile

Lists what the provider really holds, so both sides agree. Sent after `register`
and every 5 minutes.

Request: `{"instances": [...]}`, each a `fleet.Instance` from
`Provider.List`: `external_id`, `machine_id`, `running`, `tags`, `created_at`.

forecrewd compares it with its machines and answers:

```json
{"destroy": ["vm-1700"]}
```

- `destroy` lists orphans: instances with no machine, or whose machine is
  `destroyed`, `failed` or past its `deadline_at`. The agent destroys them.
- A machine in `booting`, `ready`, `busy` or `draining` that is missing from the
  list is failed with the error `gone from the provider`. A `requested` machine
  missing from it is fine: the next poll creates it.
- A `requested` machine that is in the list moves to `booting` with that
  `external_id`: the agent crashed before reporting it, and the machine keeps
  the token it booted with.

The agent reconciles before its first poll after `register`, so a machine it
started before a crash is found rather than started again with a new token.

## Sequence

```
agent                          forecrewd
  | join (code, claim, facts)  ->|   a person confirms the plan
  |<- confirmed, token ----------|
  | register (capabilities)    ->|
  | reconcile (instances)      ->|
  |<- destroy [] ----------------|
  | poll (generation)          ->|   queue demand, a machine row
  |<- machines [m1 + bootstrap] -|
  | Provider.Create(m1)          |
  | state m1 booting           ->|
  |                              |<- worker: POST /worker/v1/bootstrap, poll, ...
  | poll                       ->|   job done, machine draining
  |<- machines [] ---------------|
  | Provider.Destroy(m1)         |
  | state m1 destroyed         ->|
```
