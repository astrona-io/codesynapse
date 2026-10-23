# Single sign-on (phase 5)

Each organisation can connect one OpenID Connect provider (Entra ID, Okta, Keycloak, Google Workspace, Authentik, …). People sign in through it, get an account on first use, and the provider's groups decide their role and teams in that organisation. Passwords keep working for everyone else.

Phase 5a (this document) is the API and the rules. Phase 5b is the dashboard: the sign-in option, the callback page, the organisation's SSO settings and linked identities on the profile.

## Decisions

| Question | Decision |
|---|---|
| Library | `github.com/coreos/go-oidc/v3` for discovery and ID-token verification, `golang.org/x/oauth2` for the code exchange (both Apache-2.0/BSD). |
| Scope | One provider per organisation, configured by its owners and admins. No installation-wide provider. |
| Membership | The provider's group claim decides the role and mapped teams on every sign-in. |
| Existing accounts | Linked automatically only for a verified email that already belongs to a member of that organisation. Anyone else signs in with their password and links from their profile. |
| Root | The bootstrap root never signs in through SSO. |

## Flow

The dashboard proxy never follows redirects (`redirect: 'error'`), so no step redirects through the API:

1. The dashboard posts `POST /api/v1/auth/sso/{org}/start` (body `{"next":"/path"}`, optional). The API stores a pending state and answers `{"url": "<provider authorization URL>"}`. It also sets `cs_sso`, an HTTP-only `SameSite=Lax` cookie that binds the state to this browser for ten minutes.
2. The browser goes to the provider. The provider returns it to the dashboard page `/sso/callback`, which is `CODESYNAPSE_PUBLIC_URL` + `/sso/callback`.
3. That page posts `code` and `state` to `POST /api/v1/auth/sso/callback`. The API:
   - clears the binder cookie (it works once whatever happens);
   - takes the state;
   - exchanges the code;
   - verifies the ID token;
   - applies the rules below and sets the session cookie;
   - answers `{"next": "<path>"}`.

Linking from a signed-in profile uses `POST /api/v1/me/sso/{org}/link` the same way; the callback then requires the same signed-in user who started it.

**Protections:**
- **PKCE:** S256.
- **Nonce:** checked against the ID token.
- **State:** 32 random bytes, stored only as a SHA-256 hash and deleted when used (`DELETE … RETURNING`, so concurrent callbacks cannot both use it). It expires after ten minutes, and expired states are pruned.
- **Binder:** compared in constant time.
- **Return paths:** `next` is limited to a dashboard path (`/…`, not `//…`).

## Provider configuration

`GET/PUT/DELETE /api/v1/accounts/orgs/{org}/sso` is for owners and admins in a signed-in session; API keys cannot use it.

| Field | Meaning |
|---|---|
| `name`, `issuer`, `client_id`, `client_secret` | The issuer must be https without credentials, query or fragment. An empty secret on update keeps the stored one only while the issuer is unchanged. |
| `enabled` | Disabled providers start no sign-ins. |
| `scopes` | Extra scopes besides `openid email profile` (up to 10). |
| `groups_claim` | ID-token claim with the groups (default `groups`), as a list or a single string. |
| `default_role` | Role for anyone the provider lets in without a mapped group: guest, viewer, member or admin. Empty means only mapped groups enter. |
| `role_groups` | Group → role. The strongest mapped role wins and replaces the default. |
| `team_groups` | Group → team ID of this organisation. |

Before storing, the API checks that the issuer answers OpenID discovery through the guarded client (502 otherwise). Group and scope lists are bounded (100 mappings, 500 groups read from a token).

The client secret is sealed with AES-256-GCM under `CODESYNAPSE_SECRET_KEY`. The ciphertext is bound to purpose `oidc-client-secret`, the organisation and the issuer, so it cannot be replayed as a Git token or for another provider. Only a preview is ever returned, logged or audited. Without `CODESYNAPSE_SECRET_KEY`, SSO cannot be configured or used.

## Network

All provider traffic goes through `sso.Client`:
- **No internal addresses:** it refuses loopback, private, link-local and other internal addresses at dial time, which covers DNS rebinding. `CODESYNAPSE_SSO_ALLOW_PRIVATE=true` allows a self-hosted provider on an internal network.
- **No redirects:** it follows none.
- **Size limit:** responses are capped at 1 MiB.
- **Endpoints:** discovered endpoints must be https.
- **Timeouts:** discovery and the token exchange each time out after 20 seconds.

## Sign-in rules

Applied inside one accounts policy transaction (`CompleteSSOLogin`):

1. **Known subject.** An identity with this issuer and subject signs in its linked account, unless that account is suspended or the root.
2. **New subject:**
   - The provider must send `email_verified` (boolean or `"true"`).
   - An account with that email is linked only if it is already a member of this organisation. Another organisation's provider cannot take over someone's account (409, `ErrSSOLinkRequired`).
   - Otherwise a passwordless account is created, but only when the provider grants a role. Nobody gets an account just for trying (403, `ErrSSONoAccess`).
3. **Membership:**
   - A membership an owner added by hand is never changed by the provider.
   - A membership the provider created (`idp_memberships`) follows the granted role and mapped teams on every sign-in, and ends when no role is granted. That removal commits even though the sign-in is refused.
   - Teams the provider doesn't map are left alone.
   - The last owner is never removed or demoted by the provider: the change is refused and audited, and so is the sign-in.
4. **Session.** The session is the same `cs_session` cookie a password login gets.

Audit actions:
- `sso.login`, with success or failure and the reason;
- `sso.link` and `sso.unlink`;
- `sso.member.add`, `sso.member.role` and `sso.member.remove`;
- `user.signup`, with detail "single sign-on";
- `org.sso.set` and `org.sso.remove`.

## Identities

`GET /api/v1/me/identities` lists the signed-in user's links. `DELETE /api/v1/me/identities/{id}` removes one, but never the only way into a passwordless account. Deleting a provider keeps its identities, so reconnecting the same issuer restores sign-in.

## Storage

Migration `0011_sso.sql`:
- `identity_providers`: one per organisation; the secret is sealed.
- `external_identities`: unique per issuer and subject.
- `sso_states`: hashed state, binder hash, nonce, PKCE verifier and expiry.
- `idp_memberships`: deleted with the membership.

memstore and PostgreSQL pass the same contract (`accountstest.RunSSOStore`).

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `CODESYNAPSE_PUBLIC_URL` | `http://localhost:3000` | The dashboard's address; the redirect URI is this plus `/sso/callback`. Register it with the provider. |
| `CODESYNAPSE_SSO_ALLOW_PRIVATE` | `false` | Allow providers on private or loopback addresses. |
| `CODESYNAPSE_SECRET_KEY` | — | Required; seals client secrets. |

## Tests

- **`internal/sso`:** the guarded client (internal addresses, redirects), claims parsing, the exchange and its rejections (nonce, verifier, audience, issuer, expiry, client secret). These run against `ssotest`, a standard-library OIDC provider over TLS that checks client authentication, the redirect URI, PKCE and single-use codes.
- **`internal/accounts`:** the store contract on memstore and PostgreSQL.
- **API, `TestSSOConfiguration`:** roles, API keys, the secret preview, discovery failure.
- **API, `TestSSOSignIn`:** just-in-time accounts with no password, group mapping and its updates, removal, refusal without a role, the binder, state reuse, unverified email, the audit trail.
- **API, `TestSSOLinking`:** no takeover of accounts outside the organisation, hand-added members keep their role, root is refused, linking from the profile, unlinking, disconnecting the provider.
- **API, `TestSSOGuards`:** the only identity of a passwordless account cannot be unlinked; the last owner is neither removed nor demoted; suspended accounts cannot sign in.
