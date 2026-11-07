# Trackers

Status: contract (T72). Project boards are T73, Jira T74, setup screens and
Linear T75.

A person hands a ticket to Forecrew in the tracker the team already uses. The
ticket becomes a goal, the goal runs its normal playbook, and status goes back
to the ticket (plan decision 36). Go: `internal/tracker` is the core, which
knows no tracker; each tracker is one package under `internal/tracker/`, which
implements `tracker.Adapter` and passes `internal/tracker/trackertest`. A test
keeps the core from importing an adapter.

Deleting a connection takes its routes, identities, cursor and outbox with it,
and is refused while it has a live ticket or a write that is not sent yet:
an ended link's unsent comment would be lost with the outbox. Ended links stay, with no connection:
they are how a context pack knows a goal's text came from a ticket.

## Tickets

```go
type Ref struct {
	Connection uuid.UUID
	Key        string // the tracker's key: acme/app#12, ENG-7
}

type User struct {
	ID   string // stable: never a name or an email that can change
	Name string
	Bot  bool
}

type Ticket struct {
	Ref       Ref
	Project   string // the tracker's project: a repo, a Jira key
	Title     string
	Body      string // Markdown; adapters convert from their own format
	Type      string
	Labels    []string
	Components []string
	Assignee  User
	Reporter  User
	State     string // a name of States
	Closed    bool   // the tracker counts it as finished, done or canceled
	URL       string
	Parent    string
	Updated   time.Time
	Handovers []Handover // who handed the ticket over, newest last
	Comments  []Comment  // the latest comments, oldest first
	Issue     *RepoIssue // the GitHub issue the ticket is, if it is one
}

type Handover struct {
	Trigger Trigger // assignee, label or state
	Value   string  // the user, label or state it was set to
	By      User    // who did it; empty when the tracker cannot say
	At      time.Time
}

type Comment struct {
	ID     string
	Author User
	Body   string
	At     time.Time
}
```

`Handovers`, `Comments` and `Issue` are not in the first list of T72's prompt:
polling must give what webhooks give, so who acted and what they wrote have to
come from `Get`, not from an event's payload.

The core clips what an adapter returns: titles to 200 characters, bodies to
64 KiB, comments to the latest 100 and 16 KiB each, labels to 100.

## Adapters

```go
type Adapter interface {
	Kind() string
	Capabilities() Capabilities
	Verify(ctx context.Context) error // can this connection work? Run when it is created
	ParseEvent(r Request) ([]Event, error) // checks the signature: ErrBadSignature
	Changes(ctx context.Context, cursor string) (events []Event, next string, err error)
	Get(ctx context.Context, key string) (Ticket, error) // ErrNotFound
	Comment(ctx context.Context, key, body string) (id string, err error)
	Transition(ctx context.Context, key, state string) error
	States(ctx context.Context) ([]State, error)
}

type Capabilities struct {
	Webhooks, Assignment, Labels, Columns, Transitions, Comments bool
}

type Event struct {
	ID   string // the delivery, for deduplication; empty for polling
	Key  string // the ticket that changed
	Self bool   // Forecrew's own write, as far as the tracker says
}
```

`Capabilities` declares what the tracker can do; the core branches on it and
never on `Kind`. A trigger the adapter cannot serve is refused when the
connection is created, and a write it cannot do is left out. `Columns` is a
state or column that can hand a ticket over; `Transitions` is a state Forecrew
can write. Polling through `Changes` is mandatory, because webhooks cannot
always reach a server.

`Changes` with an empty cursor returns no events and a cursor of now: a new
connection does not read the backlog.

An optional `Linker` (`LinkedUser(ctx, accountID, user)`) finds a Forecrew user
through an identity the organization already has, as the GitHub adapter does
with linked GitHub accounts. `tracker_identities` comes first.

`tracker.Registry` holds a `Factory` per kind:
`Factory func(domain.TrackerConnection) (Adapter, error)`.

### Adding a tracker

1. A package `internal/tracker/<kind>` with an `Adapter` and a `Factory`.
2. A test that calls `trackertest.Run`.
3. `registry.Register("<kind>", factory)` in `internal/app`.
4. Credentials stay with the adapter, never in `tracker_connections.config`.

Nothing in `internal/tracker` changes. A webhook goes to
`POST /webhooks/tracker/{connection}`; a kind that shares an endpoint, as
GitHub does, calls `Service.Deliver` from it, which queues the tickets for
`Service.Run` so the caller never waits for a tracker.

## Events are hints

A webhook or poll result says which ticket changed; the core then reads the
ticket with `Get`. Nothing in a payload is believed. A forged event is refused
by the signature, and a signed one can do no more than make the core look.

Events are deduplicated by delivery, and every effect is idempotent, so a
replay changes nothing. Writes go out with a `dedupe_key`; an event marked
`Self` is dropped, a comment that came from the outbox is never read as a
person's, and a write of Forecrew's own that does get read finds nothing to do.

## Handover

A ticket starts work only when a route matches and a person handed it over,
by a trigger of its connection: `assignee` (assignment to the Forecrew user),
`label` or `state`. A trigger counts while it still holds on the ticket. There
is no background pull of a backlog. After a link has ended, only a handover
newer than its end counts, so a finished ticket that keeps its label stays
finished.

| What happened | Result |
|---------------|--------|
| No handover, or the ticket is closed | nothing |
| No route matches | Inbox, `no_route` |
| Routes to more than one project match | Inbox, `many_projects` |
| Who handed it over is unknown, or maps to no user | Inbox, `unknown_person`, with a gate |
| That user lacks `goal.create` on the project | Inbox, `not_permitted`, with a gate |
| Otherwise | queued, and started when under the limit |

The **Inbox** of tickets is `list_tickets_waiting`. A ticket with a project also
has a `ticket_intake` gate, so it shows with the other gates; a ticket without
one cannot, because a gate belongs to a project, and whoever accepts it names
the project. `accept_ticket` and approving the gate do the same, and so do
`dismiss_ticket` and rejecting it. A waiting ticket that a permitted person
hands over again starts at once.

## Routes

An ordered list for each connection. A route matches on the tracker's project,
type, labels and components; each field that is set must match, and a list
matches when the ticket has one of its values. Every route is tried: all those
that match must name one project, and the first of them gives the goal kind
and the playbook. The kind defaults by type: `bug` gives `bug`, anything else
`feature`. The playbook defaults by kind and project: a bug runs `bugfix` on a
project with the testing system (`testing.projects`), which it needs, and the
goal playbook anywhere else; other goals run the project's goal playbook.

## Limits

`trackers.max_active_goals` (default 3) limits the goals from tickets at work in
a project. Other tickets queue in order, with one comment saying so, and start
as the permission of the person who handed them over still allows; a person
who lost it sends the ticket back to the Inbox.

## Untrusted text

Ticket titles, bodies and comments are untrusted wherever they reach an agent,
whoever wrote them. A goal from a ticket shows in context packs inside
`contextpack.WrapUntrusted`, title included, with the ticket as its source; a
`ticket_intake` gate quotes the ticket the same way and keeps ticket text out
of its title. Agents get no tracker credentials and no tracker tools.
Attachments are not fetched.

A goal keeps the text it started from. An edit of the ticket never changes it:
the edit opens a `ticket_intake` gate on the goal, a possible change for a
person to read, one at a time.

## Status back

| Logical state | When |
|---------------|------|
| `queued` | waiting under the limit |
| `in_progress` | the goal is at work |
| `in_review` | a task of the goal is in review or ready to merge |
| `done` | the goal is done |
| `canceled` | the goal was canceled |

`tracker_routes.states` maps these to the tracker's states, leaving out any it
cannot do. Only `done` and `canceled` may map to a closed state: anything else
would read as a person closing the ticket, and cancel the goal. Comments carry the goal link, PR links, agents' questions and the
outcome. Every write goes through `tracker_outbox`: retried with backoff from
30 seconds up to an hour, and a tracker outage never stops a goal. A link has at
most one pending transition: a new state replaces the one waiting, so a retry
never undoes what came after it.

## Human changes win

A ticket closed or canceled by a person cancels its goal
(`workflow.Engine.CancelRun`), unless a merge is under way: a merge was approved
and has not landed, or every task is merged. Then the goal finishes, and a
comment says so. A queued or waiting ticket that closes leaves.

Approvals stay in Forecrew: a comment never resolves a gate. A mapped user may
answer an agent's `ask_human` question with `@forecrew reply <text>`; the
answer reaches the agent marked untrusted, and needs
`gate.approve.needs_you`. A comment counts once: read again after a failure it
answers nothing, and an answer that did not go through says so on the ticket. Anyone else's reply is ignored and audited.

## GitHub Issues

Kind `github`, package `internal/tracker/githubissues`, over the existing App,
its `issues` and `issue_comment` events and `/webhooks/github`. Config:

```yaml
repos: [acme/app]     # repos of projects of this organization
bot_login: forecrew[bot]   # the App's user: its events are dropped
```

The `assignee` trigger needs a real user: GitHub cannot assign an issue to an
App, so make a machine user. Who assigned or labeled comes from the issue's
timeline. Issues with `forecrew:goal`, `forecrew:task` or `forecrew:bug` are
Forecrew's and are never tickets.

A linked issue in the project's repo is the goal's issue, with no duplicate: a
ticket holds its issue from the moment it has a project, so `@forecrew bug`
refuses it, and a ticket whose issue a goal already works on ends with a comment
saying which. The spec then goes in a comment, so the person's text stays. For an issue of any
other repo, and any other tracker, `goal.intake` opens its issue as usual and
its body links the ticket. A canceled goal does not close an issue that is its
ticket: the route's `canceled` mapping does, if it has one. `@forecrew approve` and `changes` do nothing on an
issue that is a ticket, and keep working everywhere else, `github.allowlist`
included.

## GitHub Projects

Kind `github_project`, package `internal/tracker/githubproject`, over the
existing App and GitHub's GraphQL API. One connection is one board of an
organization; moving a card to the column of the `state` trigger hands it over,
and Forecrew moves the card as the goal advances. Config:

```yaml
owner: acme            # the organization that owns the board
project: 7             # the board's number
field: Status          # the single-select field that holds the columns (default Status)
closed: [Done]         # columns that count as finished (default Done)
bot_login: forecrew[bot]   # the App's user: its events are dropped
```

Only `state` is a trigger: `Verify` refuses `assignee` and `label`. The columns are the options of the field, and
`States` returns them, those of `closed` marked closed. A route's `states` name
columns; Forecrew's own move into the handover column is no handover, since an
App is not a person, but map `in_progress`, `in_review` and `done` to other
columns anyway, so a card in the handover column means a person is waiting.

- **Permissions and events.** The App needs the organization permission
  Projects (read and write), issues read and write as before, and the event
  `projects_v2_item`. GitHub sends that event for boards of an organization
  only, and only to an App with the Projects permission: no repo webhook and no
  user-owned board gets it, and an App cannot read a user's board at all. The
  issue events of `/webhooks/github` do not say which card they are about, so an
  edit of an issue's text or a comment on it reaches the core by polling.
  `Changes` reads the board and keeps the cards changed since its cursor (the
  newest of the card's, its column's and its issue's update), so it gives what
  the events give, and more. The scan asks only for each card's kind and times;
  `Get` reads the rest. It reads at most 5,000 cards (`github.MaxScanPages`): a
  larger board fails its poll with an error saying to rely on webhooks. The
  App's token for the board is narrowed to Projects (write) and Issues (read).
- **Keys.** A ticket's key is the card's node ID, stable for the life of the
  card, drafts included. `Verify` refuses a board whose owner owns no repo of one
  of the organization's projects, a board the App cannot read, and a trigger
  that is not a column.
- **Issues.** A card that is an issue has `Issue` set, so the core gives its
  link the issue's number: the card and the same issue as a ticket of a `github`
  connection cannot both have a goal. The ticket's project is the issue's repo,
  so a board of several repos is routed by `match.project`. An issue of a repo
  that is not a repo of one of the organization's projects is no ticket: `Get`
  says not found before it reads its comments or timeline, so the connection
  never reads outside the organization's projects, and one such card cannot
  fail a poll. A draft has no repo
  and no timeline: `match.project` cannot match it, and who moved it is not known,
  so it waits in the Inbox. Pull requests are not tickets.
- **Who moved it.** For an issue, the timeline event
  `ProjectV2ItemStatusChangedEvent` of the move into the handover column names the
  person, who must map to a Forecrew user as with any tracker. The event counts
  only if it is on this board, into this column, and no more than a minute
  before the time GitHub gives the card's column: a card that was removed and
  added again, or put in the column with no event, is not credited to an earlier
  move. A move by a board workflow, a move with no event, a bot and a stranger have no known person: the
  ticket waits in the Inbox with `unknown_person`.
- **Finished.** A card is closed when its issue is, when it is archived, or when
  it is in a `closed` column. A card that is deleted is no event.
- **Writes.** A comment goes on the card's issue; a draft takes none, and the
  write is dropped. A transition to a column that was renamed or deleted fails
  with an error naming the board's columns and moves nothing. The write is
  retried with the usual backoff until the column exists again: a newer state of
  the same link replaces it, but one of an ended link stays, and the connection
  cannot be deleted while it does. Fix the route's `states` before the goal ends.
