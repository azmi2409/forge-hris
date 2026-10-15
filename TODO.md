# TODO — forge-hris build plan

Tracks the 6-component MVP from the REA (`docs/rea-hris.md` §6). Check off as each component lands green with tests.

- [ ] **1. Core platform** — Organization/Company, RBAC (Owner/Admin/Manager/Member), Employee + Group + manager graph, PATs + service accounts + OAuth2 client-credentials, Postgres RLS, append-only hash-chained audit, idempotency middleware, structured errors, cursor pagination. *Everything hangs off identity + tenancy + audit.*
- [ ] **2. Time tracking** — clock in/out + breaks (idempotent), manual hour entries, pay-period lifecycle + approvals, schedule basics, **first-class shift scheduling** (Jibble's documented gap), overtime engine. Skip: face rec, GPS/geofencing, screenshots (surveillance; optional plugins later).
- [ ] **3. Leave** — policies with accrual/carry-forward/expiry, requests with snapshots, ordered approval chains, balance ledger, holiday calendars, history audit. *Most agent-invoked module; feeds payroll.*
- [ ] **4. Agent surface v1** — REST per REA §5.1; MCP server generated from handlers (§5.3 tool map, ⚠️⛔ gates enforced server-side); HMAC webhooks (§5.4); SCIM 2.0 (§5.6). Dogfood the team's own attendance via MCP.
- [ ] **5. Payroll engine** — Draft→Approved→Locked runs, item-preset catalog with statutory flags, gross→net lines, EPF/SOCSO/EIS/PCB rule packs (pluggable per-country), payslips, `preview_payroll` → `submit_payroll` gate, bank files (Maybank first), statutory exports (EPF CSV, PCB txt, Borang A). *Deliberately later: statutory correctness is the regulated core.* ⚠️ Cross-check every rule against LHDN/EPF/SOCSO sources before any real payroll run.
- [ ] **6. Integrations** — Sync Items ingestion, Xero bill-on-approval, Slack/Teams clock-in bot, payslip→ESS. ESS portal alongside.

**Out of MVP:** face recognition, GPS/geofencing, screenshots, Singapore payroll, native mobile apps (API-first; PWA later), deep Xero/QuickBooks sync.

**Open decisions (need his call):** stack — Go (recommended, consistent with forge-tasks' agent patterns) vs Rails 8; statutory scope — Malaysia only (Panda parity) vs MY+SG; deeper MY-payroll fidelity vs Gusto/Rippling-analog endpoints.
