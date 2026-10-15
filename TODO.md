# TODO — forge-hris build plan

Tracks the 6-component MVP from the REA (`docs/rea-hris.md` §6). Check off as each component lands green with tests.

- [x] **1. Core platform** — Organization/Company, RBAC (Owner/Admin/Manager/Member), Employee + Group + manager graph, PATs + scoped service accounts + OAuth2-ready auth, Postgres RLS + hash-chained audit, idempotency middleware, structured errors, cursor pagination, OpenAPI 3.1. **DONE 2026-10-16** — `store/` (types, MemoryStore, pgx Postgres adapter, 001_init.sql, ApplyMigration), `api/` (auth/RBAC/idempotency/rate-limit, 21 endpoints), `cmd/forge-hris-api`, `docs/openapi.yaml`; `go test ./... -race` green (store 7 pass/3 skip, api 11 pass + 33 subtests); CI + docker-compose + Dockerfile + DEPLOY.md landed. *Everything hangs off identity + tenancy + audit.*
- [ ] **2. Time tracking** — clock in/out + breaks (idempotent), manual hour entries, pay-period lifecycle + approvals, schedule basics, **first-class shift scheduling** (Jibble's documented gap), overtime engine. Skip: face rec, GPS/geofencing, screenshots (surveillance; optional plugins later).
- [ ] **3. Leave** — policies with accrual/carry-forward/expiry, requests with snapshots, ordered approval chains, balance ledger, holiday calendars, history audit. *Most agent-invoked module; feeds payroll.*
- [ ] **4. Agent surface v1** — REST per REA §5.1; MCP server generated from handlers (§5.3 tool map, ⚠️⛔ gates enforced server-side); HMAC webhooks (§5.4); SCIM 2.0 (§5.6). Dogfood the team's own attendance via MCP.
- [ ] **5. Payroll engine** — Draft→Approved→Locked runs, item-preset catalog with statutory flags, gross→net lines, **pluggable per-country rule packs: MY (EPF/SOCSO/EIS/PCB/HRDF + statutory forms) + ID (BPJS Kesehatan, BPJS Ketenagakerjaan JHT/JKK/JKM/JP, PPh 21 + ID statutory reports)** — statutory scope decided 2026-10-16 (Malaysia + Indonesia), payslips, `preview_payroll` → `submit_payroll` gate, bank files (Maybank first), statutory exports (MY: EPF CSV, PCB txt, Borang A; ID: BPJS + PPh 21 report formats). *Deliberately later: statutory correctness is the regulated core.* ⚠️ Cross-check every rule against LHDN/EPF/SOCSO (MY) and BPJS/DJP (ID) sources before any real payroll run.
- [ ] **6. Integrations** — Sync Items ingestion, Xero bill-on-approval, Slack/Teams clock-in bot, payslip→ESS. ESS portal alongside.

**Out of MVP:** face recognition, GPS/geofencing, screenshots, Singapore payroll, native mobile apps (API-first; PWA later), deep Xero/QuickBooks sync.

**Decided:** stack = Go (user's pick, 2026-10-16); statutory scope = **Malaysia + Indonesia** (user's pick, 2026-10-16 — MY rule pack EPF/SOCSO/EIS/PCB/HRDF, ID rule pack BPJS Kesehatan/Ketenagakerjaan, PPh 21).

**Open decisions:** deeper MY/ID-payroll fidelity (requires vendor docs/accounts) vs Gusto/Rippling-analog endpoints.
