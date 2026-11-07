# Jira

Kind `jira`, package `internal/tracker/jira`. One adapter serves **Jira Cloud**
and **Jira Data Center** installed on a company's own servers, with their
custom workflows, private certificates and context paths. The general rules
of trackers (handover, routes, the Inbox, status back) are in
[contracts/tracker.md](../contracts/tracker.md); this page is what is
particular to Jira.

When a connection is set up, the adapter reads Jira's `serverInfo` and stores
`deployment` (`cloud` or `data_center`) in the connection's config. The two
share most of the REST API; what differs is kept small:

| | Cloud | Data Center |
|-|-------|-------------|
| REST API | `/rest/api/3` | `/rest/api/2` |
| Text | Atlassian Document Format (ADF) | wiki markup |
| A user is | the account ID | the user key (not the username or email, which can be renamed) |
| Search | `search/jql`, paged by token | `search`, paged by offset |
| Sign-in | API token, or API token with scopes | personal access token (Bearer) |

**Supported Data Center versions:** 10.3 and later. 10.3 is the oldest Long
Term Support release Atlassian still supports (until 5 December 2026); a
connection to an older server is refused when it is set up. **Atlassian ends
Data Center on 28 March 2029**: the products go read-only and Atlassian stops
selling new licenses before that. This adapter is for companies still on Data
Center until they move; it shares its code with Cloud, so a move changes the
connection's address and token, not the routes.

## The account Forecrew uses

Make a **dedicated Jira account** for Forecrew, with access to the projects it
should read and nothing else. A Data Center personal access token has the
full access of its account, and a Cloud API token has the access of its
account within its scopes, so the account is what limits Forecrew.

The account needs, in each project of the connection:

| Permission | For |
|------------|-----|
| Browse projects | reading tickets, their comments and history, and searching |
| Add comments | the goal link, PR links, questions and the outcome |
| Transition issues | status back (leave the route's `states` empty to do without) |
| Assignable user | the `assignee` trigger only: people assign tickets to this account |

`forecrew tracker test <connection>` checks each of these.

### Tokens

- **Data Center:** a personal access token of the dedicated account
  (profile, Personal Access Tokens), with an expiry. `auth: pat`.
- **Cloud, the narrowest:** an API token **with scopes** of the dedicated
  account (Atlassian account, Security, API tokens, "Create API token with
  scopes", app Jira). Give it `read:jira-work`, `write:jira-work` and
  `read:jira-user`, the classic scopes Atlassian recommends; granular
  scopes work too, with those each operation of the REST reference lists for
  reading issues, comments, statuses, projects and users, and writing comments
  and transitions. `auth: scoped_token`, with `email`. Atlassian
  serves scoped tokens only through `api.atlassian.com`; the adapter reads
  the site's cloud ID when it is set up and goes there.
- **Cloud, classic:** an API token without scopes, with the full access of
  its account. `auth: api_token`, with `email`.

OAuth 2.0 (3LO) apps are not supported: a scoped API token gives the same
narrow access without an app and its refresh tokens.

Tokens live in the secret store (`internal/secrets`), never in the config:
`token_ref: store:<name>`. A configuration plan asks for the value in a secure
field and stages it until the plan is applied
([config-resources.md](../contracts/config-resources.md#secrets)).

## Setting a connection up

As configuration resources, in a plan (YAML, forms, the CLI or the
assistant):

```yaml
- kind: tracker_connection
  name: jira
  spec:
    kind: jira
    config:
      base_url: https://jira.acme.local/jira   # with its context path
      projects: [OPS, ENG]
      auth: pat
      token_ref: store:jira-token
      ca_bundle: |                              # only for a private CA
        -----BEGIN CERTIFICATE-----
        ...
        -----END CERTIFICATE-----
      webhook_secret_ref: store:jira-webhook    # only with a webhook
    triggers:
      label: forecrew
- kind: tracker_route
  name: jira/1
  spec:
    project: app
    match: {project: OPS, types: [Bug], labels: [backend]}
    states: {in_progress: In Progress, in_review: Code Review, done: Closed}
```

Once the plan is applied, run `forecrew tracker test jira` (or
`test_tracker_connection`): a plan does not reach Jira, whose token it puts in
place only when it is applied. The test reports each of these on its own,
because these are what fail on-premise:

| Check | Fails when |
|-------|-----------|
| `address` | the URL does not answer, is refused as private, or the Data Center version is too old |
| `certificate` | the server's certificate is not trusted: add its CA to `ca_bundle` |
| `login` | the token is refused, expired or unreadable |
| `projects` | the account cannot browse a project, comment on or transition its issues |
| `transitions` | the projects have no status in the done category |
| `routes` | a route maps a logical state to a status the projects do not have |

A check after a failed one says it was not checked. The first test that
passes stores the deployment, if the plan's connection did not have it yet.

`forecrew tracker connect` and `create_tracker_connection` make a connection
directly when the token is already in the secret store; they test it first.

### Config

| Key | Required | |
|-----|----------|-|
| `base_url` | yes | `https://` only, with the context path if Jira has one |
| `projects` | yes | project keys; issues of other projects are never read |
| `auth` | yes | `pat`, `api_token` or `scoped_token` |
| `email` | Cloud | the token's Atlassian account |
| `token_ref` | yes | `store:<name>` |
| `ca_bundle` | no | PEM certificates to trust besides the system's; nothing else (a private key is refused) |
| `webhook_secret_ref` | no | `store:<name>`; without it, polling only |
| `webhook_signing` | no | `hmac` (default) or `url` |
| `deployment`, `cloud_id` | | read from Jira; never set by a person |

## Addresses, certificates and proxies

- A **context path** (`https://jira.acme.local/jira`) is kept on every call.
- A **private CA** goes in `ca_bundle`, per connection. **TLS verification
  can never be turned off**, and the URL must be `https://`.
- forecrewd's **proxy** settings (`HTTPS_PROXY`, `NO_PROXY`) apply to Jira
  calls.
- **Private addresses.** By default a connection whose URL resolves to a
  private, loopback or link-local address, or redirects to one, is refused,
  checked at every request and redirect and again when the connection is
  made: on a hosted server, a connection cannot be used to reach Forecrew's
  own network. A **self-hosted** forecrewd in the same network as its Jira
  sets `trackers.private_addresses: true` in `forecrew.yaml`. Through a
  proxy, the proxy makes the connection: it should refuse internal addresses
  itself.
- Jira does **not** need to reach Forecrew: polling alone works. A Jira with
  no outside access at all and a hosted Forecrew need a relay, which is not
  built yet.

## Handover and routes

Triggers, as for any tracker:

- `label`: a label added to the ticket (the changelog says who added it).
- `assignee`: assignment to the dedicated account. Set it to that account's
  **account ID** on Cloud, or its **user key** on Data Center
  (`/rest/api/2/myself` shows both; `forecrew tracker test` shows the ID).
- `state`: a move to a status, by its name.

Routes match on project key, issue type, labels and components. **Custom
fields are not matched.** To route on one, add a label instead: by hand, or
with a Jira automation rule ("When: field value changed", "Then: edit issue,
add label `team-payments`") and match the label.

## Status back and workflows

A route's `states` map logical states to **status names** of the project,
which `States` reads from the projects' statuses; a status in the `done`
category counts as closed. Companies change their workflows, so for each move
the adapter reads the transitions the ticket offers and takes the one that
reaches the status. It forces nothing: when no transition reaches the status,
when the transition needs a field Forecrew does not fill (a screen with a
required field and no default), or when a validator refuses it, Forecrew
comments on the ticket, sets the route's `warning` (shown by `forecrew tracker
routes` and `list_tracker_routes`) and does not retry. The next move that goes
through clears the warning. Make the transition available from every status
in the workflow, or give its fields defaults, and the warning goes away.

## People

A person is their account ID on Cloud and their user key on Data Center, so a
renamed username or a changed email stays the same person. Map people with
`forecrew tracker map <connection> <account ID or user key> <user>`; a
person confirms each mapping. Matching people by email to suggest mappings is
not built yet: Cloud hides most emails.

## Events: polling and webhooks

**Polling** is the default and needs nothing in Jira. Every two minutes
forecrewd searches with JQL, `project in (...) AND updated >= "<time>"`,
from the last change it saw minus five minutes: JQL compares minutes, in the
**time zone of the token's user**, which the adapter reads and uses. A rate
limit (`429`) or an unavailable server with `Retry-After` is waited out when
it asks for a minute or less, and otherwise left to the next poll or to the
outbox's retry.

**Webhooks** make it faster where someone sets them up; every ticket is read
again anyway, so a webhook is only a hint. Store a random secret
(`webhook_secret_ref`) and register
`https://<forecrew>/webhooks/tracker/<connection ID>`:

- **Cloud:** Jira settings, System, WebHooks: the URL, the secret, a JQL
  filter of the connection's projects, and the events issue created, issue
  updated and comment created. Jira signs each delivery (`X-Hub-Signature`,
  HMAC-SHA256).
- **Data Center:** Administration, System, WebHooks, with the same events.
  Where the version's webhook form has a secret field (it does in 11.x; check
  "Managing webhooks" in the documentation of yours), set it: Jira signs the
  same way. Where it has none, use the secret URL below.
- **A sender that cannot sign**, such as an automation rule's "Send web
  request": set `webhook_signing: url` and send to
  `https://<forecrew>/webhooks/tracker/<connection ID>?secret=<the secret>`.
  The URL is the secret: keep it out of logs and screenshots. forecrewd logs
  the path only.

A delivery about a change by the token's own user is Forecrew's own write and
is dropped, once the adapter knows that user (from its first poll).

## Text

Ticket bodies and comments are converted to Markdown, from ADF on Cloud and
from wiki markup on Data Center; Forecrew's comments are written in the
format the deployment takes. Panels, expands, tables, mentions, emoji, cards,
statuses and macros keep their text; a node or macro the converter does not
know keeps its text or, without any, its name in brackets, and is never
dropped silently. Images and attachments are not fetched. All of it is
untrusted text ([tracker.md](../contracts/tracker.md#untrusted-text)).

## Not supported

Jira Service Management portals, custom fields in routes, attachments, OAuth
2.0 apps, and a relay for a Jira with no outside access.
