# forge-hris

Open-source HRIS built **for AI agents** — a Jibble × PayrollPanda clone where agents are first-class operators, not bolted-on integrations.

- **Time tracking** — clock in/out, breaks, hour entries, pay-period lifecycle (Open → Submitted → Approved/Rejected → Reopened), overtime engine, schedules with first-class shift scheduling.
- **Leave** — time-off policies with accrual/carry-forward/expiry, requests with snapshots, ordered approval chains, balance ledger, holiday calendars.
- **Payroll** — Draft → Approved → Locked runs, payroll-item presets with per-item statutory flags, gross→net lines, pluggable per-country rule packs (Malaysia: EPF/SOCSO/EIS/PCB first), payslips, bank-file generation, statutory exports.
- **Agent-ready by design** — OpenAPI 3.1 source of truth, MCP server generated from the same handlers (`hris.*` tools, approval-gated for money/PII), HMAC webhooks, SCIM 2.0, append-only hash-chained audit with agent identity, `Idempotency-Key` on all POSTs, two-phase `preview` → `submit` for irreversible operations.

Full reverse-engineering analysis: [`docs/rea-hris.md`](docs/rea-hris.md).

## Status

Components 1–5 done (core platform, time tracking, leave, agent surface,
payroll engine); component 6 (integrations) remains. Build progress in
[`TODO.md`](TODO.md).

## Build plan (summary)

1. Core platform — org/tenant, RBAC, employees, PATs + service accounts, Postgres RLS, audit, idempotency.
2. Time tracking — punches, breaks, hour entries, pay periods, scheduling, overtime.
3. Leave — policies, accrual engine, approvals, balances, calendars.
4. Agent surface v1 — REST + MCP + webhooks + SCIM.
5. Payroll engine — runs, statutory rule packs, payslips, bank files, exports.
6. Integrations — Sync Items, Xero bill-on-approval, Slack/Teams bots, ESS portal.

## Quickstart

Prerequisites: Go 1.27+.

```bash
# Run the API (in-memory store; data is lost on restart)
go run ./cmd/forge-hris-api
```

Bootstrap the first organization — the only unauthenticated mutating route.
The owner token is shown **once**; save it.

```bash
RESP=$(curl -s -X POST localhost:8080/api/v1/organizations \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Sdn Bhd","slug":"acme","country_code":"MY",
       "owner":{"first_name":"Azmi","last_name":"Muwahid","email":"azmi@acme.test"}}')
TOKEN=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
ORG=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["organization"]["id"])')

curl -s localhost:8080/healthz
# {"status":"ok"}

curl -s "localhost:8080/api/v1/organizations/$ORG/employees" \
  -H "Authorization: Bearer $TOKEN" | head -c 300
```

Time tracking and leave (components 2–3). Issue a PAT for the owner —
clock in/out and break operations require an `Idempotency-Key` header.

```bash
OWNER=$(echo "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["employee"]["id"])')
PAT=$(curl -s -X POST "localhost:8080/api/v1/organizations/$ORG/credentials" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"kind":"pat","name":"ops-agent","employee_id":"'"$OWNER"'"}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')

# Clock in (idempotent: replay with the same key is safe)
curl -s -X POST "localhost:8080/api/v1/organizations/$ORG/clock-in" \
  -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: punch-20261016-01' \
  -d '{"employee_id":"'"$OWNER"'"}'

# Create a leave policy, then request + approve leave
POL=$(curl -s -X POST "localhost:8080/api/v1/organizations/$ORG/leave-policies" \
  -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: pol-20261016-01' \
  -d '{"name":"Annual Leave","accrual_mode":"monthly","accrual_amount":1.25}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# Unpaid here because a fresh employee has no accrued balance yet; paid
# requests check the balance ledger and return 409 INSUFFICIENT_BALANCE.
REQ=$(curl -s -X POST "localhost:8080/api/v1/organizations/$ORG/leave-requests" \
  -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: req-20261016-01' \
  -d '{"employee_id":"'"$OWNER"'","policy_id":"'"$POL"'",
       "start_date":"2026-12-24","end_date":"2026-12-25","is_unpaid":true}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')

# Approve (chainless request: the owner decides as admin)
curl -s -X POST "localhost:8080/api/v1/organizations/$ORG/leave-requests/$REQ/approve" \
  -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: appr-20261016-01' \
  -d '{"version":1}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["request"]["status"])'
# approved
```

Payroll (component 5): draft → preview → submit. Money movement is never
single-step — the submit gate requires a live preview payload.

```bash
# Draft a run for October 2026 (one run per YYYY-MM period)
RUN=$(curl -s -X POST "localhost:8080/api/v1/organizations/$ORG/payroll-runs" \
  -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: run-202610-01' \
  -d '{"period":"2026-10","country_code":"MY"}' \
  | python3 -c 'import json,sys; d=json.load(sys.stdin)["payroll_run"]; print(d["id"], d["version"])')
RUN_ID=$(echo "$RUN" | cut -d' ' -f1); RUN_VER=$(echo "$RUN" | cut -d' ' -f2)

# Preview: dry-run payroll.Calculate over the inputs (all amounts in sen),
# diff against the previous run with anomaly flags, save the 30-minute
# preview payload. (POST .../calculate is the lines-only dry-run variant;
# preview additionally folds approved unpaid leave + staged sync items.)
PV_ID=$(curl -s -X POST "localhost:8080/api/v1/organizations/$ORG/payroll-runs/$RUN_ID/preview" \
  -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: pv-202610-01' \
  -d '{"employees":[{"employee_id":"'"$OWNER"'","base_salary_minor":500000,
       "age_years":30,"citizen_my":true,"bank_account":"1234567890","bank_code":"MBB"}]}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["preview_id"])')

# Submit: draft -> approved; fans payroll.submitted + one payslip.issued per line
curl -s -X POST "localhost:8080/api/v1/organizations/$ORG/payroll-runs/$RUN_ID/submit" \
  -H "Authorization: Bearer $PAT" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: submit-202610-01' \
  -d '{"preview_id":"'"$PV_ID"'","version":'"$RUN_VER"'}' \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["payroll_run"]["status"], d["payslips_issued"])'
# approved 1

# Payslip + statutory exports for the run
curl -s "localhost:8080/api/v1/organizations/$ORG/payroll-runs/$RUN_ID/payslips/$OWNER" \
  -H "Authorization: Bearer $PAT" | head -c 200
curl -s "localhost:8080/api/v1/organizations/$ORG/payroll-runs/$RUN_ID/exports/epf-csv" \
  -H "Authorization: Bearer $PAT" | head -3
```

The same gate is available to agents via the MCP tools (`hris.preview_payroll`
→ `hris.submit_payroll`, two-phase with resume token + `confirmed:true`);
see [`docs/payroll.md`](docs/payroll.md) for the rule packs, the verification
warning, and the scopes/RBAC matrix.

Against Postgres (schema is auto-applied on boot):

```bash
export DATABASE_URL=postgres://forge:forge@localhost:5432/forge_hris?sslmode=disable
go run ./cmd/forge-hris-api
```

Or everything with one command: `docker compose up -d` (see [DEPLOY.md](DEPLOY.md)).
Full API contract: [`docs/openapi.yaml`](docs/openapi.yaml).

## Agent surface (component 4)

The MCP server exposes 29 `hris.*` tools over stdio or HTTP — same store
calls as the REST API, same Bearer <redacted> model, with server-side approval
gates: `confirm`-tier tools (creates, deactivations, approvals) return
`approval_required` + a resume token instead of executing; `never`-tier
(`hris.approve_leave_bulk`) additionally requires `confirmed:true`.
Every tool call is audited as `actor_type=agent`, and tool mutations emit
webhooks like their REST counterparts. Full catalog: [`docs/mcp.md`](docs/mcp.md).

```bash
# stdio (Claude Desktop / Claude Code) — token via env
export FORGE_HRIS_MCP_TOKEN="$PAT"
go run ./cmd/forge-hris-mcp --stdio

# HTTP: POST JSON-RPC to /mcp, SSE stream on GET /mcp
go run ./cmd/forge-hris-mcp --addr :8081
curl -s -X POST localhost:8081/mcp -H 'Content-Type: application/json' \
  -H "Authorization: Bearer $PAT" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | head -c 300
```

Webhooks: `POST /api/v1/organizations/$ORG/webhooks` (admin) with
`{"url":"https://example.com/hooks","events":["leave.requested"]}` —
deliveries are HMAC-SHA256 signed (`X-Hub-Signature-256`), at-least-once.
SCIM 2.0 for Okta/Entra: provision a service-account credential, then point
the IdP at `/scim/v2` (see `docs/openapi.yaml`).

## License

MIT. See [LICENSE](LICENSE).
