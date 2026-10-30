# Create an MCP token and connect a client

The same `fcm_...` token works for MCP (`/mcp`), the REST API (`/api/v1`) and
the `forecrew` CLI. It belongs to one account; only its hash is stored.

## Create

```sh
docker compose exec forecrewd forecrew token create alice-laptop
```

The token is printed once. Use one token per person or client, named after the
person and device: the name is the actor recorded on everything the token does
(`mcp:alice-laptop`).

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

`401` means the token is wrong or revoked.

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
secrets are not stored and cannot be shown. A revoked token gets `401` at once,
and so do the web sessions it backs. You may revoke the token you are using; it
is the last request that token makes. To replace a leaked token, revoke it and
create a new one: creating tokens needs a shell on the server
(`forecrew token create`).
