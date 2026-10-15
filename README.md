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

## License

MIT. See [LICENSE](LICENSE).
