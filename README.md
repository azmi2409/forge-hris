# forge-hris

Open-source HRIS built **for AI agents** — a Jibble × PayrollPanda clone where agents are first-class operators, not bolted-on integrations.

- **Time tracking** — clock in/out, breaks, hour entries, pay-period lifecycle (Open → Submitted → Approved/Rejected → Reopened), overtime engine, schedules with first-class shift scheduling.
- **Leave** — time-off policies with accrual/carry-forward/expiry, requests with snapshots, ordered approval chains, balance ledger, holiday calendars.
- **Payroll** — Draft → Approved → Locked runs, payroll-item presets with per-item statutory flags, gross→net lines, pluggable per-country rule packs (Malaysia: EPF/SOCSO/EIS/PCB first), payslips, bank-file generation, statutory exports.
- **Agent-ready by design** — OpenAPI 3.1 source of truth, MCP server generated from the same handlers (`hris.*` tools, approval-gated for money/PII), HMAC webhooks, SCIM 2.0, append-only hash-chained audit with agent identity, `Idempotency-Key` on all POSTs, two-phase `preview` → `submit` for irreversible operations.

Full reverse-engineering analysis: [`docs/rea-hris.md`](docs/rea-hris.md).

## Status

Pre-build. The REA is complete; component implementation follows the plan in [`TODO.md`](TODO.md).

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

Against Postgres (schema is auto-applied on boot):

```bash
export DATABASE_URL=postgres://forge:forge@localhost:5432/forge_hris?sslmode=disable
go run ./cmd/forge-hris-api
```

Or everything with one command: `docker compose up -d` (see [DEPLOY.md](DEPLOY.md)).
Full API contract: [`docs/openapi.yaml`](docs/openapi.yaml).

## License

MIT. See [LICENSE](LICENSE).
