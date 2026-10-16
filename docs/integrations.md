# Integrations — Xero, Slack, Teams, ESS

Component 6 of forge-hris. The machine contracts live in
[`docs/openapi.yaml`](openapi.yaml) (v1.4.0, 12 integration paths); this doc
is the operator guide: how to set each integration up, which secrets it
needs, and where the data flows.

> **Caveat (honest):** no sandbox testing was done for Xero, Slack, or Teams
> in this repo — real developer credentials are needed to verify any of
> these end-to-end. The contracts below are fixed so the handler packages
> (Worker A: `api/xero.go`, `integrations/xero`, `integrations/secrets`;
> Worker B: `api/chat.go`, `integrations/slack`, `integrations/teams`,
> `web/ess`) can be built against them independently.

## Overview

| Integration | Direction | What it does |
|---|---|---|
| Xero | outbound | Posts approved payroll runs as Xero ACCPAY bills (dry-run preview first) |
| Slack | inbound + outbound | `/clockin` / `/clockout` slash commands and event callbacks resolve to employees |
| Teams | inbound | Webhook receiver for Teams adaptive-card clock in/out actions |
| ESS | web UI | Employee self-service portal: magic-link login, timesheets, leave, payslips |

All org-scoped integration routes require `Authorization: Bearer <token>`
with the **admin** role (or owner); the inbound chat webhooks and the Xero
OAuth callback are the exceptions — they carry their own verification
(Slack signing secret, Teams shared secret, Xero `state` token) and take no
forge-hris credential.

## Secrets

Integration secrets (Xero OAuth tokens, Slack signing secret + bot token,
Teams shared secret) are stored **encrypted at rest** with an AES-256-GCM
envelope keyed by the server environment variable:

```bash
# 32 random bytes, base64-encoded (generate once, keep it out of git)
export FORGE_HRIS_SECRET_KEY="$(head -c 32 /dev/urandom | base64)"
```

- The key must be exactly 32 bytes after base64-decoding.
- Rotate by re-encrypting: the integrations API exposes no plaintext
  secrets after registration — every secret is shown once at registration
  time and is never returned again.
- Losing the key means losing the ability to decrypt stored secrets
  (re-register each integration); the key itself cannot be recovered.

## Xero

Bill-on-approval: when a payroll run is approved, forge-hris can post its
totals to Xero as a supplier bill (ACCPAY), driven by your account and
contact mappings.

### App setup (developer.xero.com)

1. Create an app → choose **OAuth 2.0**.
2. Set the redirect URI to
   `https://<host>/api/v1/integrations/xero/callback` (must match
   `XERO_REDIRECT_URL` exactly, including scheme).
3. Tick the scopes `accounting.transactions`, `accounting.settings`, and
   `offline_access` (the last keeps the refresh token alive so bills can
   post without an admin re-login).
4. Copy the client ID and client secret into the server environment:

```bash
export XERO_CLIENT_ID="..."
export XERO_CLIENT_SECRET="..."
export XERO_REDIRECT_URL="https://<host>/api/v1/integrations/xero/callback"
```

### Connect flow

1. Admin calls `POST /api/v1/organizations/{orgID}/integrations/xero/connect`
   → `{authorization_url, state}`.
2. Redirect the admin's browser to `authorization_url`; they pick the Xero
   organisation and approve.
3. Xero redirects to `/api/v1/integrations/xero/callback?code=…&state=…`.
   A `state` mismatch is rejected with `400 INVALID_STATE`. On success the
   tokens are exchanged and stored encrypted; the response is
   `{connected: true, tenant_id}`.

### Mappings

`POST /api/v1/organizations/{orgID}/integrations/xero/mappings` (admin):

```json
{
  "auto_post": true,
  "account_mappings": [
    {"component_key": "sys:basic_salary", "account_code": "400"},
    {"component_key": "sys:epf_employer", "account_code": "403"}
  ],
  "contact_mappings": [
    {"employee_id": "<emp-id>", "contact_id": "<xero-contact-id>"}
  ]
}
```

- `component_key` is a payroll item preset code (system seeds look like
  `sys:basic_salary`); `account_code` is the Xero chart-of-accounts code.
- `auto_post: true` makes **approving** a payroll run post the bill to Xero
  automatically (the submit gate still requires the preview → submit
  two-phase flow first). The approval itself never fails on Xero errors:
  failures are recorded as `failed` ledger rows (visible via the bill
  endpoint response and the `xero.bill_failed` webhook) and the run stays
  approved.

### Posting a bill

`POST /api/v1/organizations/{orgID}/payroll-runs/{id}/xero-bill`
(admin, `Idempotency-Key`):

- `{"dry_run": true}` → returns the bill JSONs that *would* be posted, **one
  per employee** (each employee is one Xero contact), without touching Xero.
  Safe to call any time; use it to sanity-check mappings.
- `{"dry_run": false}` → posts one ACCPAY bill per employee and returns
  `{"postings": [...]}` — per-bill ledger rows (the source of truth for
  retries). Total failure is a 502 `XERO_API_ERROR` with the failed rows.
  Replaying with the same idempotency key returns the stored result instead
  of posting twice.
- Only **approved** runs can post: anything else is rejected with 409
  `XERO_RUN_NOT_APPROVED`, so a draft's tentative numbers can never reach
  Xero.

`DELETE /api/v1/organizations/{orgID}/integrations/xero` (admin) revokes the
tokens and clears tenant/mappings/auto-post. Bills already posted in Xero
are left untouched.

## Slack

Clock in/out from Slack via slash commands backed by the time-tracking
engine.

### App setup (api.slack.com)

1. Create an app (from scratch) for the workspace.
2. **Slash commands** → create `/clockin` and `/clockout`, both pointing at
   `https://<host>/api/v1/integrations/slack/events`.
3. **Event Subscriptions** → enable, set the request URL to the same
   `/api/v1/integrations/slack/events` (Slack sends a `url_verification`
   challenge; the endpoint echoes it), and subscribe to the bot events the
   integration needs.
4. **Interactivity & Shortcuts** → set the request URL to
   `https://<host>/api/v1/integrations/slack/events` if the integration
   uses interactive components.
5. Copy the **signing secret** (Basic Information page) and install the app
   to get the **bot token** (`xoxb-…`, OAuth & Permissions page).

### Register the workspace

Admin calls `POST /api/v1/organizations/{orgID}/integrations/slack`:

```json
{
  "team_id": "T012345",
  "team_name": "Acme",
  "signing_secret": "…",
  "service_token": "xoxb-…"
}
```

Both secrets are stored encrypted and never returned again. Then link users
so commands resolve to employees:

```json
POST /api/v1/organizations/{orgID}/integrations/slack/users
{"team_id": "T012345", "slack_user_id": "U012345", "employee_id": "<emp-id>"}
```

`team_id` is required — one organization can connect multiple Slack
workspaces, so the link target is ambiguous without it.

Inbound requests to `/api/v1/integrations/slack/events` are verified with
the workspace's signing secret (`X-Slack-Signature`); they carry no
forge-hris credential. Unlinked Slack users get an ephemeral "not linked"
reply.

## Teams

Clock in/out from Teams via an incoming webhook carrying adaptive-card
actions.

1. In the Teams app manifest set
   `webhookUrl: https://<host>/api/v1/integrations/teams/webhook?key=<secret>`
   where `<secret>` is a strong random string you generate.
2. Admin registers it: `POST /api/v1/organizations/{orgID}/integrations/teams`
   with `{"tenant_ref": "<teams-tenant>", "shared_secret": "<secret>"}`.
   The secret is stored encrypted and never returned again.
3. Link users: `POST /api/v1/organizations/{orgID}/integrations/teams/users`
   with `{"teams_user_id": "<aad-object-id>", "employee_id": "<emp-id>"}`.

The inbound webhook takes no forge-hris credential — the `key` query
parameter must equal the registered shared secret, otherwise the request is
rejected with `401`.

## ESS

The employee self-service portal is server-rendered HTML mounted at
`/ess/` (Worker B, package `web/ess`):

- **Login** — magic link: the employee enters their work email, the server
  emails a single-use signed link (15-minute expiry). No passwords.
- **Pages** — home (upcoming shifts, pending approvals), timesheets
  (clock in/out, breaks, history), leave (balances, request, approvals),
  payslips (per approved run).
- **Mailer** — the mailer is pluggable: the handler takes a `Mailer`
  interface so deployments can plug in SMTP, SES, or a stub. The default
  build logs the magic link instead of sending it — set a real mailer
  before exposing `/ess/` publicly.

ESS is intentionally *not* part of the JSON API surface; it reuses the same
`store.Store` and RBAC checks as the REST handlers.

## Endpoint index

| Method & path | Auth | Purpose |
|---|---|---|
| `POST /api/v1/organizations/{orgID}/integrations/xero/connect` | admin | start OAuth2 → `{authorization_url, state}` |
| `GET /api/v1/integrations/xero/callback` | Xero `state` | finish OAuth2 → `{connected, tenant_id}` |
| `GET /api/v1/organizations/{orgID}/integrations/xero/status` | any role | `{connected, tenant_id, auto_post}` |
| `POST /api/v1/organizations/{orgID}/integrations/xero/mappings` | admin | account + contact mappings, auto-post flag |
| `DELETE /api/v1/organizations/{orgID}/integrations/xero` | admin | revoke tokens, clear mappings |
| `POST /api/v1/organizations/{orgID}/payroll-runs/{id}/xero-bill` | admin + Idempotency-Key | dry-run preview or post bill |
| `POST /api/v1/integrations/slack/events` | Slack-signed | events + `/clockin` `/clockout` |
| `POST /api/v1/organizations/{orgID}/integrations/slack` | admin | register workspace + secrets |
| `POST /api/v1/organizations/{orgID}/integrations/slack/users` | admin | link Slack user → employee |
| `POST /api/v1/integrations/teams/webhook?key=` | shared secret | Teams card actions |
| `POST /api/v1/organizations/{orgID}/integrations/teams` | admin | register tenant + secret |
| `POST /api/v1/organizations/{orgID}/integrations/teams/users` | admin | link Teams user → employee |
