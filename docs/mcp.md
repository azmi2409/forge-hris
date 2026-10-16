# MCP server (component 4)

The forge-hris agent surface: a JSON-RPC 2.0 MCP server (`mcp/` package,
`cmd/forge-hris-mcp`) exposing 29 `hris.*` tools over the **same store
calls the REST handlers use**, so REST and MCP never drift. Protocol
version `2024-11-05`.

## Tool catalog

| Tool | Tier | Scope / role | Description |
|---|---|---|---|
| `hris.list_employees` | auto | `employees:read` | List employees (filter group/status/search, paginated) |
| `hris.get_employee` | auto | `employees:read` | Get one employee (members: self only) |
| `hris.create_employee` | **confirm** | `employees:write` | Create employee |
| `hris.update_employee` | auto | `employees:write` | Sparse update, version-checked |
| `hris.deactivate_employee` | **confirm** | `employees:write` | Soft-deactivate (status → removed) |
| `hris.list_departments` | auto | `employees:read` | List departments/groups |
| `hris.get_org_chart` | auto | `employees:read` | Departments + manager graph + direct reports |
| `hris.clock_in` | auto | `time:write` | Open a time entry (member: self) |
| `hris.clock_out` | auto | `time:write` | Close the open entry; idempotent with `idempotency_key` |
| `hris.start_break` | auto | `time:write` | Start a break inside the open entry |
| `hris.end_break` | auto | `time:write` | End the open break |
| `hris.get_timesheet` | auto | `time:read` | One pay period + entries in range + open entry |
| `hris.list_timesheets` | auto | `time:read` | List pay periods (filter definition/status) |
| `hris.submit_timesheet` | auto | manager+ | Submit (Open → Submitted; entries freeze) |
| `hris.approve_timesheet` | **confirm** | manager+ | Approve (locks every entry covered) |
| `hris.reopen_timesheet` | **confirm** | admin+ | Reopen (Approved → Reopened; unlocks entries) |
| `hris.correct_time_entry` | auto | `time:write` | Superseding correction (original frozen, never edited) |
| `hris.list_leave_types` | auto | `leave:read` | List time-off policies |
| `hris.get_leave_balance` | auto | `leave:read` | Recalculated balance (tenure, requests, carry, expiry) |
| `hris.request_leave` | auto | `leave:write` | Request leave (balance-checked; default chain = manager) |
| `hris.cancel_leave_request` | auto | `leave:write` | Cancel (requested/approved → cancelled) |
| `hris.approve_leave` | **confirm** | `leave:approve` | Approve (current chain step) |
| `hris.approve_leave_bulk` | **never** | `leave:approve` | Bulk approve, all-or-nothing, two-phase (see below) |
| `hris.reject_leave` | **confirm** | `leave:approve` | Reject (current chain step) |
| `hris.who_is_on_leave_today` | auto | `leave:read` | Approved leave overlapping a date (default today) |
| `hris.get_attendance_report` | auto | `time:read` | Entries over a range + open entry |
| `hris.get_leave_report` | auto | `leave:read` | Requests + balance ledger rows |
| `hris.get_audit_log` | auto | `audit:read` | Hash-chained audit log (filter actor/action/type/since) |
| `hris.get_overtime_report` | auto | `time:read` | Overtime buckets over a range (mirrors `POST /overtime/preview`) |

No payroll tools: those land with component 5.

Tool annotations (`readOnlyHint` / `destructiveHint` / `idempotentHint` /
`openWorldHint`) are advertised in `tools/list` but are **hints only**; the
server enforces tiers itself. Each `tools/list` item also carries
`_meta.forge = {tier, requiredScope, minRole}`.

## Approval flow

```
agent                          MCP server                        human
  | -- tools/call hris.approve_leave {request_id, version} --> |
  |                                                            |
  | <-- {approval_required:true, resume_token, preview} ------- |
  |     (NOTHING executed; token single-use, 10-min TTL)       |
  |                                                            |
  |   human reviews preview, says "yes"                        |
  |                                                            |
  | -- tools/call same args + resume_token + approved:true --> |
  | -- executes; audit event appended (actor_type=agent) ----> |
```

- **confirm** tier (`create_employee`, `deactivate_employee`,
  `approve_timesheet`, `reopen_timesheet`, `approve_leave`,
  `reject_leave`): first call returns `approval_required:true` with a
  preview and a single-use resume token; the follow-up re-sends the same
  arguments plus `resume_token` and `approved:true`. The token binds to the
  approved argument snapshot — the follow-up executes exactly those
  arguments.
- **never** tier (`approve_leave_bulk`): same, but the follow-up must
  additionally carry `confirmed:true`. The first call's preview includes
  per-request approvability **counts** (`total` / `approvable` /
  `blocked`); execution is all-or-nothing via
  `BulkApproveTimeOffRequests`.
- Bad/expired/reused tokens return structured errors
  (`APPROVAL_NOT_FOUND`, `APPROVAL_EXPIRED`, `APPROVAL_NOT_CONFIRMED`)
  with `isError:true`; the store is untouched until approval.
- Previews never mutate and never write audit events.

## Auth

Same bearer PAT / service-account model as the REST API:

- The presented token is digested with `hex(sha256(token))` and looked
  up via `store.GetCredentialByTokenHash`. Unknown, expired, and revoked
  tokens are rejected identically.
- PATs inherit their employee's role scopes (`scopesForRole`); service
  accounts carry their explicit scope list. Service accounts are treated
  as the highest role whose scope set they fully cover (same
  `effectiveRole` rule as REST).
- The credential's organization is the tenant — tools take no `org_id`.
- **stdio:** `FORGE_HRIS_MCP_TOKEN` env, or the `token` field of
  `initialize` params.
- **HTTP:** `Authorization: Bearer <token>` on every request.

## Audit identity

Every successful tool execution appends one audit event with
`actor_type: "agent"`, `actor_id` = the credential ID, and optional
`on_behalf_of` (pass the `on_behalf_of` argument for delegated `act_as`;
both identities land in the log). Mutations use the REST audit taxonomy
(`employee.created`, `time_entry.clock_in`, `leave_request.approved`,
`pay_period.submitted`, …); read tools use `<resource>.listed` /
`<resource>.read` / `*_report.generated` actions in the same
`<resource>.<verb>` style.

## Transports

- **stdio** (default): newline-delimited JSON-RPC 2.0 on stdin/stdout.
  `initialize` is required before `tools/*` (`-32002` otherwise); the
  token may arrive via env or `initialize.params.token`.
- **HTTP** (`--addr`, e.g. `:8081`): `POST /mcp` takes one JSON-RPC
  request and returns the JSON-RPC response (notifications → `202`,
  batching is rejected). `GET /mcp` is a Server-Sent Events stream:
  one `ready` event, then server notifications, with `: ping`
  heartbeats every 30s. Limits: stateless (no session affinity), no
  `Last-Event-ID` resumability, one broadcast domain per process.

## Elicitation

The server advertises the `elicitation` capability but does **not**
implement server→client elicitation: the approval resume flow replaces
it and works identically on stdio and HTTP. `Server.Elicit` returns a
structured `ELICITATION_NOT_SUPPORTED` error directing callers to the
resume-token flow.

## Running

```bash
# stdio (Claude Desktop / Claude Code), in-memory store
FORGE_HRIS_MCP_TOKEN=hris_... go run ./cmd/forge-hris-mcp

# HTTP on :8081 with Postgres
DATABASE_URL=postgres://... go run ./cmd/forge-hris-mcp --addr :8081
```

## Claude Desktop config

```json
{
  "mcpServers": {
    "forge-hris": {
      "command": "/path/to/forge-hris-mcp",
      "args": ["--stdio"],
      "env": { "FORGE_HRIS_MCP_TOKEN": "hris_..." }
    }
  }
}
```

For the HTTP transport instead:

```json
{
  "mcpServers": {
    "forge-hris": {
      "command": "npx",
      "args": ["-y", "mcp-remote", "https://hris.example.com/mcp",
               "--header", "Authorization: Bearer hris_..."]
    }
  }
}
```

## Claude Code

```bash
# stdio
claude mcp add forge-hris \
  --env FORGE_HRIS_MCP_TOKEN=hris_... \
  -- /path/to/forge-hris-mcp --stdio

# HTTP
claude mcp add --transport http forge-hris https://hris.example.com/mcp \
  --header "Authorization: Bearer hris_..."
```

Mint the token via the REST API (`POST
/api/v1/organizations/{orgID}/credentials`), then keep it in the
environment — the server only ever sees its SHA-256 hash.
