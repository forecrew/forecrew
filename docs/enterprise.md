# Enterprise build

What a company needs to adopt Forecrew: verified domains with SSO enforcement,
just-in-time provisioning, identity provider groups mapped to teams, SCIM and
custom roles (plan decision 27). The identity model is in
[contracts/identity.md](contracts/identity.md), the permissions in
[contracts/permissions.md](contracts/permissions.md).

## The boundary

The code is in `ee/` at the root of the repository, under its own license
(`ee/LICENSE`, decided in S.6). Every Go file there has the build tag
`enterprise`, so the open-core build does not compile it.

```sh
make build             # open core
make build-enterprise  # with ee/ linked in
```

`make test`, `make lint` and `make cover` run both builds. A test
(`internal/enterprise`) walks the repository and fails when anything outside
`ee/` imports it; the one exception is `cmd/forecrewd/enterprise.go`, behind
the tag, which is how the binary links `ee/` in.

Open core reaches `ee/` through three small interfaces, which `ee/` registers
at startup with `enterprise.Register`:

| Interface | Used by | Without `ee/` |
|-----------|---------|---------------|
| `mcp.EnterpriseTools` | MCP, REST and the CLI, through `internal/tools` | the tools exist and answer `unavailable` (`503`) |
| `auth.Policy` | sign-in: JIT and group mapping | invitations only, no groups |
| `http.Handler` | `/scim/v2/` | not mounted |

`ee/` also runs one loop, the daily check of verified domains.

**Enforcement is read in every build.** `store.EnforcedConnections` and
`store.PersonalTokenCap` are open core, as sign-in discovery already was: only
the enterprise build can turn enforcement on, but a move to the open-core
build must not lift it silently. Turn it off first.

`GET /api/v1/me` says `"enterprise": true` in the enterprise build, for the UI.

## Verified domains

`forecrew domains add acme.com` shows a TXT record:

```
_forecrew-challenge.acme.com TXT "forecrew-verification=<token>"
```

Create it, then `forecrew domains verify acme.com`. A domain may be pending in
several organizations and verified in one. The token is public, not a secret.

Forecrew looks every verified domain up at startup and every day after. When
the DNS answers that the record is gone, the domain is unverified, which ends
its enforcement. That is audited as `domain.verification_lost`, logged as an
error, and the Owners are told through a `needs_you` gate on one of the
organization's projects, the one thing Forecrew puts in front of people; an
organization without a project has only the audit log. A lookup that fails says nothing
about the record, so a DNS outage changes nothing.

## SSO enforcement

`forecrew domains enforce acme.com okta` requires one connection, the
organization's own or an instance connection, of everyone whose primary email,
or the email of one of their identities, is in the domain:

- Sign-in through any other connection does not open that organization, and is
  refused when it is the only one the person has. Their other organizations
  are not concerned.
- A session from another connection ends at its next request, and cannot
  switch into the organization.
- `@forecrew` commands from their linked GitHub account stop acting as them
  there, unless the enforced connection is GitHub.
- Legacy token login (`auth.token_login`) is refused with `403` `sso_required`,
  and a cookie from an earlier one stops working: a token proves no connection.
- Their personal tokens last `max_token_ttl`, from when they were created, so
  older tokens are limited too. Turning enforcement on sets it to 7 days when
  there is no cap; an Owner allows longer with `forecrew domains token-ttl
  720h`. Creating a token asks for at most the cap. Service account tokens are
  not limited.

Instance admins and break-glass sessions (without a connection) are exempt.
Each such entry is audited as `sso.exempted`.

## Just-in-time provisioning and group mapping

`forecrew provisioning okta --jit --default-role member --group-claim groups`
sets both on an organization's own connection.

**JIT:** a sign-in through the connection by someone who is not a member
creates the membership (source `jit`) with the default roles. The provider
decides who that is, so a member removed by hand comes back at their next
sign-in unless the provider stops them too; suspending them keeps them out.

**Group mapping:** `forecrew group-mappings add okta devs core` maps a group to
a team. At every sign-in, for each team mapped through that connection, the
person joins with source `idp` when the claim names a mapped group and leaves
otherwise. Manual and SCIM members are untouched. It applies in each
organization where the person is an active member. A provider that leaves the
claim out names no groups; Entra ID leaves it out too when there are too many
for a token and says so in `_claim_names`, and then the teams stay as they are.
Removing a team's last mapping takes its `idp` members out.

Who may set these is in permissions.md, No escalation.

## SCIM 2.0

`forecrew scim-tokens create okta` gives the URL and the bearer token for the
identity provider: `https://<forecrew>/scim/v2/<org-slug>`. It serves
`ServiceProviderConfig`, `Schemas`, `ResourceTypes`, `Users` and `Groups`, with
`eq` filters on `userName` and `externalId` (Users) and `displayName` and
`externalId` (Groups), and `startIndex` and `count` (at most 200). The server is
our own subset rather than a library: providers use little of the RFCs, and
what matters here, the reach of a token, is ours anyway.

- **A token reaches only what it provisioned:** members and teams with source
  `scim`. Anyone else does not exist for it.
- **`userName` is the email and must be in a verified domain of the
  organization.** Sign-in links by proven email, and only there does the
  organization's word on an email count. The user is created verified, so their
  first sign-in links to them. Someone with that email who is in no way a member
  of the organization yet joins with the user they have.
- **Someone who is a member already** (manual, invited, JIT) is `409
  uniqueness`, so the provider shows the clash and a person decides. To bring
  them under SCIM, remove them from the organization and provision them again.
- **Deactivating** (`active: false`) suspends the membership, ends the sessions
  in the organization and revokes the personal tokens and OAuth grants, in one
  transaction. `active: true` lets them back in, without the tokens.
- **Deleting** does the same and takes them out of the SCIM groups. The
  membership stays suspended so that the history keeps its author; the resource
  answers `404`, and creating the same `userName` brings it back.
- The last Owner cannot be deactivated (`409 mutability`).
- Groups are teams with source `scim`. A group's `members` are those SCIM added:
  replacing them leaves manual members alone, and only provisioned users can be
  added.

Attributes Forecrew does not keep are ignored. Quirks handled: `op` in either
case (Okta lowercase, Entra ID capitalised), `active` as the string `"False"`,
`PATCH` without a path and with an object as value, `members[value eq "..."]`
paths and `"$ref": null`. `PATCH` on a group answers `204`.

## Custom roles

`forecrew roles create auditor --scope organization --permission audit.read`
makes a role from the permission catalog. Whoever creates, changes or deletes
one needs `role.manage` and every permission involved, on the organization: a
change reaches everyone bound to the role. A role in use cannot be deleted:
bound, given by a pending invitation, or a JIT default of a connection.

## SAML

Forecrew is an OpenID Connect relying party and never parses SAML. Put a broker
in front: Keycloak, Zitadel or Dex speaks SAML to the company's identity
provider and OIDC to Forecrew, which sees a generic `oidc` connection.

[deploy/examples/keycloak-broker](../deploy/examples/keycloak-broker) is a
working Keycloak realm, tested by `deploy/tests/keycloak_broker_test.sh`, whose
ID token carries a verified email and the `groups` claim:

1. Start it and add your SAML provider under Identity providers in the realm.
   Turn **Trust email** on, or the emails arrive unverified. Add a mapper from
   the SAML group attribute to Keycloak groups for group mapping.
2. Run Keycloak behind TLS with a real client secret, and turn the client's
   direct access grants off: the example has them on for its test.
3. Add the connection in Forecrew: kind `oidc`, issuer
   `https://<keycloak>/realms/forecrew`, client ID `forecrew`, the secret as
   `env:FORECREW_CONN_KEYCLOAK`. Allow
   `https://<forecrew>/auth/<connection id>/callback` as redirect address.
4. `forecrew provisioning keycloak --group-claim groups`.

Zitadel and Dex work the same way: a SAML identity provider on their side, an
OIDC client for Forecrew, and groups in the ID token (`groups` with Dex; a
claim from an action with Zitadel).
