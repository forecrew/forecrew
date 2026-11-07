# Linear

Kind `linear`, package `internal/tracker/linear`. A ticket is a Linear issue,
its key is the identifier (`ENG-7`) and its project, for routes, is the
**team's key**. The general rules of trackers (handover, routes, the Inbox,
status back) are in [contracts/tracker.md](../contracts/tracker.md); this page
is what is particular to Linear. Adding the adapter changed nothing in
`internal/tracker`: it is one package and one `Register` call.

## The account Forecrew uses

Make a **dedicated Linear user** for Forecrew, a member of the teams it should
read and nothing else, and make a **personal API key** for it (Settings, Account,
Security and access). The user is what limits Forecrew: the key has that user's
access. A member can create a key; an admin is not needed. Linear gives personal
keys 2,500 requests an hour, which polling every two minutes stays far under.

Tokens live in the secret store, never in the config: `token_ref: store:<name>`.
A configuration plan asks for the value in a secure field and stages it until the
plan is applied ([config-resources.md](../contracts/config-resources.md#secrets)).

An **OAuth access token** works as well (`auth: oauth_token`, sent as a Bearer
token), for a company that issues one; Forecrew does not run the OAuth flow or
refresh the token.

## Why handover is by assignment to a user, not by agent delegation

Linear's own way to hand work to an agent is to **delegate** an issue to an
app: an OAuth application installed with `actor=app` and the `app:assignable`
scope, which Linear notifies by an *agent session* webhook. That needs a
workspace admin to install an OAuth app, a client secret, token refresh and a
second webhook type, and a token of that kind is the only one that can be a
delegate. Forecrew has no OAuth app flow for trackers (as for Jira), so the
`assignee` trigger is **assignment to the dedicated user**, which any member can
do, and `label` and `state` work as for any tracker. Delegation to an app is not
built.

## Setting a connection up

As configuration resources, in a plan (YAML, forms, the CLI or the assistant):

```yaml
- kind: tracker_connection
  name: linear
  spec:
    kind: linear
    config:
      teams: [ENG, OPS]
      token_ref: store:linear-key
      webhook_secret_ref: store:linear-hook   # only with a webhook
    triggers:
      label: forecrew
- kind: tracker_route
  name: linear/1
  spec:
    project: app
    match: {project: ENG, labels: [backend]}
    states: {in_progress: In Progress, in_review: In Review, done: Done}
```

| Key | Required | |
|-----|----------|-|
| `teams` | yes | team keys; issues of other teams are never read |
| `auth` | no | `api_key` (default) or `oauth_token` |
| `token_ref` | yes | `store:<name>` |
| `webhook_secret_ref` | no | `store:<name>`, the webhook's signing secret; without it, polling only |

Triggers: `label` (the label's name), `assignee` (the dedicated user's **ID**,
which `forecrew tracker test` prints; not the name) and `state` (a workflow
state's name).

`forecrew tracker test <connection>` (or `test_tracker_connection`) reports each
check on its own:

| Check | Fails when |
|-------|-----------|
| `address` | Linear does not answer, or the address is refused as private |
| `certificate` | the certificate is not trusted (never turned off) |
| `login` | the key is refused, expired or unreadable |
| `projects` | the user does not see a team of `teams` |
| `transitions` | the teams have no completed or canceled state |
| `routes` | a route maps a logical state to a state the teams do not have |

Linear's API cannot say which permissions a key has, so the test cannot tell a
read-only key from a full one: a write that Linear refuses shows later, as a
failed write with its error, retried by the outbox.

## Routes

Routes match on the **team key** (`match.project`), labels and types. Linear has
no issue types and no components: a ticket with the label `Bug` has the type
`Bug` (Linear's own default label), so the goal kind defaults to `bug`; any
other label is matched with `labels`. Custom fields and Linear projects are not
matched.

## Status back

A route's `states` map logical states to **workflow state names**, which
`States` reads from the teams; `completed` and `canceled` states count as
closed. Linear lets an issue move to any state of its team, so Forecrew moves it
directly and forces nothing; a name the team does not have makes the move
`blocked` (a comment on the ticket, a warning on the route, no retry), and a
name that two teams give different types is treated as open. An issue already in
the state is left alone.

## People

A person is their Linear user ID, so a rename or a changed email stays the same
person. Map people in Settings, Integrations, or with `forecrew tracker map
<connection> <user ID> <user>`.

## Events: polling and webhooks

**Polling** needs nothing in Linear. Every two minutes forecrewd reads the issues
of the teams updated since the last change it saw (a minute of overlap), and the
issues with a comment edited since, so a `@forecrew reply` is read even if
Linear does not touch the issue's own update time.

**Webhooks** make it faster where someone sets them up; every ticket is read
again anyway. In Linear: Settings, API, New webhook. The URL is
`https://<forecrew>/webhooks/tracker/<connection ID>`, the resources **Issue**
and **Comment**, and the team or all public teams. Linear shows the **signing
secret**: store it as `webhook_secret_ref`. Linear signs each delivery with
HMAC-SHA256 of the body (`Linear-Signature`) and dates it
(`webhookTimestamp`); forecrewd refuses a delivery that is unsigned, wrongly
signed or more than a minute old. Only workspace admins can create webhooks.
Linear retries a failed delivery three times and may turn the webhook off if
the URL stays unreachable.

A delivery whose actor is the token's own user is Forecrew's own write and is
dropped, once the adapter knows that user (from its first call).

## Calls and limits

- The endpoint is Linear's, `https://api.linear.app/graphql`; it is not a
  setting. TLS is always verified and a redirect that changes scheme, host or
  port is refused. forecrewd's proxy settings apply.
- A rate limit (HTTP 429 or the error code `RATELIMITED`, with `Retry-After`) of
  a read is waited out when it asks for a minute or less. A **write** (a comment
  or a state change) is never tried again within a call, so it cannot happen
  twice; the outbox retries it.
- A query asks at most 100 comments and 100 history entries of an issue, and
  reads at most 1,000 issues and 1,000 comments of a poll. The comment's text and
  the description are Markdown, which Linear stores as Markdown; they go through
  unchanged and are untrusted text
  ([tracker.md](../contracts/tracker.md#untrusted-text)). Images and
  attachments are not fetched.

## Not verified against a real workspace

No real Linear account was available, so the adapter is built against a fake
that follows the documentation. [smoke-live.md](../runbooks/smoke-live.md#linear)
lists what to confirm: the `history` fields (`toAssigneeId`, `addedLabels`,
`toState`), the comment and issue `updatedAt` filters and their ordering,
whether a comment moves an issue's `updatedAt`, the webhook payloads' issue
identifier, and the shape of rate-limit answers.

## Not supported

Delegation to an agent app and its session webhooks, OAuth flows and token
refresh, Linear projects and cycles in routes, custom fields, attachments, and
issues of teams not in `teams`.
