# Create an MCP token and connect a client

The same `fcm_...` token works for MCP (`/mcp`), the REST API (`/api/v1`) and
the `forecrew` CLI. It belongs to one organization and one owner, a person or a
service account; only its hash is stored. A request with it can do what its
scopes allow and its owner may do, so it stops working when the owner is disabled
or is no longer an active member.

## Create

With a token you have, or from the API while signed in in a browser:

```sh
forecrew tokens create alice-laptop --scope write --ttl 720h
forecrew tokens create deploy --service-account ci --scope read   # needs token.manage
```

On the server, with shell access:

```sh
docker compose exec forecrewd forecrew token create alice-laptop --user alice@example.com --scope write
docker compose exec forecrewd forecrew token create deploy --service-account ci --scope read
```

The token is printed once. `--scope` is `read`, `write` or `admin`. Through the API
it is no higher than the owner's roles allow or than the token you call with, and
a read token cannot create tokens. `--ttl` defaults to
`auth.token_default_ttl` (90 days), at most a year through the API. Use one token per device, named after it.
What the token does is recorded as its owner, with the token as the credential.

Treat the token like a deploy key. Anyone who has it can dispatch jobs, and
`dispatch_job` prompts are trusted by design (see the
[security notes](install.md#security-notes)).

## Connect a client

Claude Code:

```sh
claude mcp add --transport http forecrew https://forecrew.example.com/mcp \
  --header "Authorization: Bearer $FORECREW_TOKEN"
claude mcp list
```

Claude Desktop (through `mcp-remote`) and a team `.mcp.json` are described in
[mcp.md](../mcp.md). The Claude web and mobile connectors cannot send a static
`Authorization` header and cannot be used.

Check the connection by asking the client to call a read-only tool (for
example listing projects), or:

```sh
curl -fsS -H "Authorization: Bearer $FORECREW_TOKEN" https://forecrew.example.com/api/v1/me
```

`401` means the token is wrong, expired or revoked, or its owner lost access.

## List and revoke

With any valid token, from anywhere (the `list_tokens` and `revoke_token` MCP
tools do the same):

```sh
forecrew tokens                       # name, ID, status, last used; marks the token you use
forecrew tokens revoke alice-laptop   # a name or an ID
```

On the server, with shell access and no token:

```sh
docker compose exec forecrewd forecrew token list
docker compose exec forecrewd forecrew token revoke alice-laptop
```

The list shows each token's name, ID, status and when it was last used; the
secrets are not stored and cannot be shown. A revoked token gets `401` at once.
You may revoke the token you are using; it is the last request that token makes.
To replace a leaked token, revoke it and create a new one.
