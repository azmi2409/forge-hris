# REA Report: Agent-Ready HRIS Clone (Jibble × PayrollPanda)

**Date:** 2026-10-16 | **Method:** public web only, no logins.
**Commissioned by:** user — open-source HRIS clone of Jibble (time tracking/attendance) + PayrollPanda (payroll), **agent-ready** so his team can operate it via AI agents.
**Tagging:** [verified] = seen on vendor site/docs/live API · [inferred] = deduction from public evidence · [proposed] = clone design · [third-party] = review/aggregator sites.

---

## 1. The targets

**Jibble** — cloud time-and-attendance SaaS by Jibble Group Sdn Bhd, founded 2016, Kuala Lumpur [third-party]. Positioning: "100% free… free forever for unlimited users", ~4.8/5 across 20,000+ reviews claimed [verified: jibble.io]. Deliberate philosophy: "do one thing REALLY well instead of several HR things fairly well" — explicitly *not* a full HRIS [verified: jibble.io FAQ]. So the attendance half of the clone is Jibble; the payroll half is its sister product.

**PayrollPanda** — cloud payroll/HR by Payroll Panda Sdn Bhd, founded 2016, Petaling Jaya, Malaysia [verified: Capterra, Wellfound]. Sister product of Jibble (same company) [verified: job ads]. Core pitch: "LHDN-approved" Malaysian payroll, 1,000+ SMEs claimed, 4.7–4.8 ratings [verified]. **Country coverage: Malaysia ONLY** [verified: KB FAQ] — aggregator "multi-country" claims are noise [inferred]. **No public API at all** — no docs, keys, webhooks, or developer portal [verified negative] — so the payroll agent surface is greenfield design work.

---

## 2. Jibble — reverse-engineered in depth

### 2.1 Verified features

- **Real-time clock in/out** + automated timesheets + **offline mode** (`TimeEntry.isOffline`) [verified].
- **Kiosk modes:** NFC, face recognition, PIN, "Speed Mode"; `SpeedClockIn` API action [verified].
- **Break tracking:** paid/unpaid `BreakType`, auto-deductions [verified].
- **Overtime engine:** daily / double / weekly / rest-day / public-holiday thresholds + multipliers [verified].
- **Anti-buddy-punching:** AI face recognition (`isFaceRecognized`/`faceSimilarity`), geofencing with `isOutsideGeofence`, GPS stamps, device lock [verified].
- **Live GPS breadcrumbs** + `WhoIsWorkingNow` [verified].
- **Scheduling:** Fixed / Flexible / Weekly schedules with expected hours — but a 2026 review flags **no true shift-scheduling**; a gap to exploit [verified/third-party].
- **Time-off suite:** `TimeOffPolicy` with accrual engine (Annually/Monthly/PayPeriod/PayrollHours), carry-forward + expiry; `TimeOffInterval` requests; multi-step `TimeOffApproval` chains; `LeaveBalance` ledger; `TimeOffHistory` audit; public-holiday `Calendar`s [verified].
- **Pay-period lifecycle:** Open → Submitted → Approved/Rejected (→ Reopened), with `ChangeStatus` / `RecalculatePayPeriods` actions [verified].
- **Projects / activities / clients** with Xero invoice push; manual `HourEntry`s; reports + XLS/CSV export; desktop activity screenshots (2/5/10 min) [verified].
- **Apps:** iOS, Android (100k+ installs), desktop (Mac/Windows, screenshots, offline, auto clock in/out on wake/sleep), web, Chrome extension [verified: jibble.io/apps].

### 2.2 Most agent-ready of the products researched

Jibble is listed in **Claude's Connectors Directory** (one-click OAuth) and works with ChatGPT/Cursor: clock in/out, log hours, timesheets, time-off requests/balances, reports via natural language, role-scoped (Member/Manager/Admin/Owner) [verified: jibble.io/claude-time-tracking]. Its API even tags `TimeEntryClientType.AiAssistant` — agents are a first-class client type, not an afterthought. This is the benchmark the clone must match.

### 2.3 Public API (the best cloning artifact)

Public docs at `docs.api.jibble.io` (no login) [verified]: REST+JSON with **OData** `$filter/$orderby/$select/$expand`; async export (`Prefer: respond-async` → 202 + exportId → poll → 303 to pre-signed S3 download; 5 concurrent max); a test environment [verified].

- **Auth:** personal access tokens (`Authorization: Bearer <token>`) [verified]; OAuth2 client-credentials [third-party]; MCP via OAuth [verified].
- **Data model** (live unauthenticated `$metadata`, the single best artifact for cloning) [verified]: multi-tenant under `Organization`; GUID keys; `propagationTimestamp` sync markers; soft deletes; **snapshots frozen onto pay periods/leave requests** (auditability pattern worth copying); manager graph on `PersonComposition` (groupId, position/employmentType, scheduleId, calendarId, payPeriodDefinitionId, locationId, timeZone, workStartDate, status ∈ NotInvited/PendingInvitation/Joined/Removed).
- **Entity sets:** People, Organizations, TimeEntries, HourEntries, PayPeriods, Groups, Projects, Activities, Locations, LocationEntries, Schedules, Calendars, PayPeriodDefinitions, TimeOffPolicies/Intervals/Approvals/Attachments/Histories, LeaveBalances, TimeOffAccrualCycles, Screenshots, WhoIsWorkingNow, AsyncExports, MetaEntities, MetaRestrictions — plus actions (SpeedClockIn, ChangeStatus, RecalculatePayPeriods, AmendEntitlement, **TestWebhook**) [verified]. Webhooks exist but are lightly documented [inferred].
- **Plan gating** is metadata-driven (`MetaEntities`/`MetaRestrictions`) [inferred].
- **Integrations:** Slack, MS Teams, Xero (invoice push), QuickBooks Online, **Deel**, Zapier, and a **PayrollPanda export** (hours + leave + overtime sync; payslips published back) [verified].

### 2.4 Architecture

Backend **.NET/C# ASP.NET Core + OData** (confirmed live in `$metadata`) [verified]; 2020–21 job ads: .NET Core, EF Core 5, **PostgreSQL, RabbitMQ, AWS**, OpenAPI [third-party, consistent]; metadata-driven core (`Jibble.Common.Meta`) [inferred]; separate identity service [third-party].

### 2.5 Pricing

[verified] **No public pricing page** — `/pricing` and `/upgrade-plans` redirect to homepage; only "Free forever · Unlimited users" messaging (corroborated Oct 2026). Third-party paid-tier reports conflict (Premium ≈ $3.49–$5.99/user/mo, Ultimate ≈ $6.99–$10.99/user/mo, Enterprise custom w/ SSO/self-hosting) — do not quote without verification.

---

## 3. PayrollPanda — reverse-engineered in depth

### 3.1 Verified features

- **Payroll runs** with month lifecycle **Draft → Approved → Locked** (locked blocks syncs) [verified].
- **Payroll item presets with per-item statutory flags** — the key design to clone: e.g. `Advance Payment` subject to all deductions vs `Advance (Payment Made via Payroll)` reducing contribution wages; `Zakat (Deducted From Payroll)` vs `Zakat (Paid Direct By Employee)` [verified]. Statutory behavior is *derived from item flags + worker status*, never hard-coded per payroll.
- **Sync Items ingestion:** external batches (e.g. altHR "Expense Claims") attach to an employee's payroll month [verified].
- **ePayslips** — downloadable/emailed/ESS, publishable to Jibble [verified].
- **Bank file generation** — post-approval files for manual bank-portal upload (Maybank, CIMB, HL, RHB, Public Bank, OCBC, UOB, Alliance, AmBank, HSBC; formats like MY PAY PIPE V4 / MY STAT BODY PIPE; crediting date; Organisation Code/Originator ID). Note: "integration" = **file generation, not bank API debits** [verified].
- **Free eLeave module** — apply/approve, manager approvers, unpaid leave auto-deducts into payroll [verified]; ESS portal [verified].
- **Onboarding/offboarding** with **CP22, CP22A, CP21, TP3** forms [verified].
- No native time/attendance (via Jibble) or native claims (via altHR) — the integration story, not the monolith story [verified].
- Payroll summary/variance/custom reports; multi-company master login; role-based admin access [verified].
- **No mobile app** (KB: desktop-web product) [verified].

### 3.2 Statutory coverage — Malaysia [verified: help content]

- **PCB/MTD** auto-calculated monthly (employer-borne option; SOCSO/EIS as non-recurring in formula; e-Data PCB / e-PCB Plus txt).
- **EPF** by age/residency (foreign mandatory from 1 Oct 2025; i-Akaun CSV).
- **SOCSO** Employment Injury vs Invalidity categories with **age-threshold auto-updates**; **EIS** 0.2%+0.2%; HRDF.
- Forms: Borang A, Form 8A, CP34, Form E, EA, CP22/CP22A/CP21, TP3; Perkeso Assist exports.

### 3.3 Pricing (public site, 2026-10)

**RM 38/month base per company + RM 8/month per employee** [verified: payrollpanda.my MIA page, partner page], SST-exclusive; 15% yearly discount; >100 employees custom; invoiced after 2nd payroll run — **1st run free**; FPX/bank transfer only. Free tier: HR system "free forever for unlimited users"; eLeave free [verified] — exact limits unpublished [gap].

### 3.4 Integrations & data model

- Integrations: altHR, Jibble, QuickBooks, Xero, Zoho People + 10 bank-file formats [verified]. Xero is deepest: OAuth, **auto-post payroll as Bills on approval**, component→account/contact mapping, consolidated or per-contact [verified: Xero App Store, Nov 2025].
- Data model (all [inferred] from UI labels): Company (bank details, statutory ref numbers); User/Admin RBAC + ESS; Employee (worker status driving statutory: citizen/PR/foreigner, age bands; Payment Information); PayrollItem preset catalog (subject-to / reduces-wages-subject-to flags); PayrollRun month (Draft→Approved→Locked; per-employee lines: gross, additions, deductions, employer/employee statutory splits, net); Payslip; LeaveRequest/LeaveBalance; StatutoryFiling artifacts; BankFile; Xero mapping table.
- Architecture: thin — WordPress marketing, HubSpot CTAs, app at `app2.payrollpanda.my`, internal "PayrollPanda Core" name; backend unknown [gap].

---

## 4. Unified domain model (clone target)

Everything under `tenant_id` (company) with Postgres RLS. GUID/ULID keys. Soft deletes everywhere. **Snapshots over edits**: freeze the relevant state onto leave requests, pay periods, and payroll runs (auditability pattern lifted from Jibble). Statutory settings are *derived* (worker status + age band + item flags), never stored. All statuses are audited state machines.

**Core entities:**
- **Employee/Person** — employeeNumber, position, employmentType, department Group, manager graph, timeZone, workStartDate, invitation status.
- **Group/Department**; **Role/Permission** (Owner/Admin/Manager/Member).
- **TimeEntry** — punch: In/Out/StartBreak; project/activity/location/kiosk FKs; GPS + address; face/geofence flags (plugin territory); clientType incl. `ai_assistant`; prev/next linked list; lock flag.
- **HourEntry** (manual); **PayPeriod/Definition** (Monthly/TwiceInMonth/OnceInTwoWeeks/Weekly; Open→Submitted→Approved/Rejected→Reopened + audit); **Schedule** (incl. overtime complex type, breaks, auto-deductions); **Location/LocationEntry**; **Project/Activity/Client**.
- **TimeOffPolicy** (accrual engine: Annually/Monthly/PayPeriod/PayrollHours; carry-forward; expiry); **TimeOffRequest** (snapshots); **TimeOffApproval** (ordered chain); **LeaveBalance** ledger; **Holiday Calendar**; **TimeOffHistory**.
- **PayrollRun** (Draft→Approved→Locked); **PayrollItem preset** (statutory flags); **EmployeePayLine** (gross/additions/deductions/employer-employee statutory splits/net); **Payslip**; **StatutoryFiling**; **BankFile**; **PaymentInformation**; **SyncItem batch**.
- **AuditEvent** (immutable, hash-chained); **WebhookSubscription**.

---

## 5. Agent-ready design (the differentiator)

Jibble already treats `AiAssistant` as a first-class API client type and ships in Claude's Connectors Directory [verified]. The clone must meet or beat that from day one.

### 5.1 API surface
- REST+JSON, cursor pagination, consistent filtering (pick OData-style or plain params — one choice, consistent). **OpenAPI 3.1 as source of truth; MCP tools generated from the same handlers** so REST and MCP never drift.
- Async bulk exports via `Prefer: respond-async` (Jibble pattern) [verified].
- Resource namespaces modeled on BambooHR's time-tracking API (`clock-ins`, `clock-outs`, `clock-entries`, `hour-entries`, `timesheets`, `timesheet-approvals`) [verified: documentation.bamboohr.com].

### 5.2 AuthN/Z — three credential kinds
1. **User PATs** inheriting RBAC (BambooHR model [verified]).
2. **Service accounts** with explicit scopes (Jibble per-person bearer [verified]).
3. **OAuth2 client-credentials.**
- Scopes: `time:read/write`, `leave:read/write/approve`, `payroll:read/preview/submit`, `employees:read`, `audit:read` — **`payroll:submit` never bundled with preview**, admin-granted.
- Tenant isolation at credential level (Gusto one-token-per-company [verified: docs.gusto.com]) + RLS safety net.
- Delegated `act_as` with both identities in the audit log (Deel's Organization/Worker/Public MCP tool tiers are the model [verified: developer.deel.com]).

### 5.3 MCP tool map (`hris.*`; ⚠️ approval-gated, ⛔ never auto-approved)
- **Identity:** `list_employees`, `get_employee`, `create_employee` ⚠️, `update_employee` (version-checked), `deactivate_employee` ⚠️ (soft), `list_departments`, `get_org_chart`.
- **Time:** `clock_in`, `clock_out` (idempotent), `start_break`/`end_break`, `get_timesheet`, `list_timesheets`, `submit_timesheet`, `approve_timesheet` ⚠️, `reopen_timesheet` ⚠️, `correct_time_entry` (corrections, never edits).
- **Leave:** `list_leave_types`, `get_leave_balance`, `request_leave`, `cancel_leave_request`, `approve_leave` ⚠️, `approve_leave_bulk` ⚠️⛔ (two-phase), `reject_leave` ⚠️, `who_is_on_leave_today`.
- **Payroll:** `list_payroll_runs`, `get_payroll_run`, `draft_payroll_run`, `preview_payroll` (dry-run with diff + anomaly flags — **must precede submit**), `submit_payroll` ⚠️⛔, `cancel_payroll_run` ⚠️, `get_payslip` (own only for workers), `list_pay_schedules`.
- **Reports:** `get_attendance_report`, `get_leave_report`, `get_payroll_summary`, `get_audit_log`.
- **Gate rule:** self reads/writes auto-approvable; anything affecting others, PII writes, or money movement = human confirmation. Encode via MCP annotations (`readOnlyHint`/`destructiveHint`/`idempotentHint`/`openWorldHint` [verified: MCP spec]) **plus server-side enforcement** — annotations are hints, the server is the gate.

### 5.4 Webhooks
HMAC-SHA256 (`X-Hub-Signature-256`), CloudEvents envelope, unique message IDs, at-least-once delivery (Deel ships HMAC webhooks [verified]; BambooHR has none — avoid polling [verified]). Taxonomy `<resource>.<verb>`: `employee.created/updated/deactivated`, `clock_in.created`, `timesheet.submitted/approved/reopened`, `leave.requested/approved/rejected/balance_changed`, `payroll.drafted/calculated/submitted/cancelled`, `payslip.issued`, `api_token.created/revoked`.

### 5.5 Auditability
Append-only **hash-chained** `audit_events`: actor_type ∈ {human, agent, system, scim}, `on_behalf_of`, before/after JSONB, idempotency key. Agent identity first-class (Scalekit's Gusto MCP per-call trail [verified]). Approvals are audited events. Money/approvals immutable once committed — reversals, not edits. Per-tenant retention; anonymize-don't-delete.

### 5.6 Agent-first API principles
- `Idempotency-Key` on all POSTs (24h; **409 on fingerprint mismatch** — prevents double clock-in / double payroll submit).
- Structured errors `{code, message, details, request_id}` with stable UPPER_SNAKE codes; standard `429`+`Retry-After` (never BambooHR's 503-for-rate-limit footgun).
- Optimistic concurrency → `409 VERSION_CONFLICT` (Gusto pattern [verified]; same as forge-tasks).
- **Two-phase commits everywhere irreversible:** `preview` then `submit` (Gusto calculate→submit [verified]; Rippling full-timecard payroll preview + run diffs [verified]).
- **SCIM 2.0** (`/scim/v2/Users`, `/Groups`, `externalId`, `active:false` soft-deactivate, enterprise extension) for provisioning [verified: learn.microsoft.com].
- MCP `outputSchema` + **elicitation** for human-in-the-loop on ⚠️⛔ tools [verified: MCP spec].

---

## 6. Build plan (MVP order)

1. **Core platform (wks 1–2)** — Organization/Company, RBAC (Owner/Admin/Manager/Member), Employee + Group + manager graph, PATs + service accounts + OAuth2 client-credentials, Postgres RLS, append-only audit, idempotency middleware, structured errors, cursor pagination. *Everything hangs off identity + tenancy + audit.*
2. **Time tracking (wks 3–4)** — clock in/out + breaks, manual hour entries, pay-period lifecycle + approvals, schedule basics, **first-class shift scheduling** (Jibble's documented gap), overtime engine. Skip: face rec, GPS/geofencing, screenshots (surveillance; optional plugins later).
3. **Leave (wks 5–6)** — policies with accrual/carry-forward/expiry, requests with snapshots, ordered approval chains, balance ledger, holiday calendars, history audit. *Most agent-invoked module; feeds payroll.*
4. **Agent surface v1 (wk 7)** — REST per §5; MCP server generated from handlers; HMAC webhooks; SCIM. Dogfood the team's own attendance via MCP.
5. **Payroll engine (wks 8–11)** — Draft→Approved→Locked runs, item-preset catalog with statutory flags, gross→net lines, EPF/SOCSO/EIS/PCB rule packs (pluggable per-country), payslips, `preview_payroll` → `submit_payroll` gate, bank files (Maybank first — best documented), statutory exports (EPF CSV, PCB txt, Borang A). *Deliberately later: statutory correctness is the regulated core.*
6. **Integrations (wk 12+)** — Sync Items ingestion, Xero bill-on-approval, Slack/Teams clock-in bot, payslip→ESS. ESS portal alongside.

**Out of MVP:** face recognition, GPS/geofencing, screenshots, Singapore payroll (Panda has none — greenfield scope), native mobile apps (API-first; PWA later), deep Xero/QuickBooks sync.

---

## 7. Key cloning decisions carried forward

- **Jibble parity:** copy the OData entity model shape (via the public `$metadata`), pay-period lifecycle, time-off accrual engine, and the metadata-driven restriction pattern for plan gating.
- **PayrollPanda parity:** copy the item-preset statutory-flag catalog, Draft→Approved→Locked run lifecycle, Sync Items ingestion, bank-file formats (not bank APIs), and the full MY statutory rule pack (EPF/SOCSO/EIS/PCB/HRDF + forms).
- **Differentiation:** first-class shift scheduling (Jibble gap); an agent surface that ships *with* the MVP instead of bolted on years later; SCIM + audit-grade webhooks that BambooHR-class products lack.

## 8. Open questions for the user

1. PayrollPanda has no public API — payroll agent endpoints follow Gusto/Rippling analogs. Is deeper MY-payroll fidelity needed (would require their docs/account)?
2. Statutory scope: Malaysia only (Panda parity) or MY+SG from the start?
3. Repo name: **forge-hris** (recommended — matches forge-ci / forge-tasks) vs forge-people?
4. Stack: Go (recommended — consistency with forge-tasks' agent-ready patterns, high-load I/O) vs Rails 8 (his day-job stack)?

## 9. Caveats

- Jibble has no public pricing page (paid prices are conflicting third-party reports); PayrollPanda has no public API at all (payroll agent surface is greenfield).
- Jibble's shift-scheduling depth is disputed; Panda's overtime/proration/encashment formulas and backend stack are unpublished.
- The unauthenticated `$metadata` endpoint is the single best artifact for cloning Jibble's data model.
- All statutory rules verified against PayrollPanda's public help content; cross-check against LHDN/EPF/SOCSO sources before any real payroll run — statutory correctness is legal liability, not just a bug.

## 10. Sources

jibble.io (+ /claude-time-tracking, /apps, /help/*), docs.api.jibble.io, time-tracking.prod.jibble.io/v1/$metadata, payrollpanda.my (+ /hr-system, /integrations-payroll/, /help/, /salary-calculator/), apps.xero.com/my/app/payrollpanda, help.althr.my, capterra.com, documentation.bamboohr.com, docs.gusto.com, developer.deel.com, learn.microsoft.com (SCIM), MCP spec, rippling.com, ontheclock.com / connecteam.com (Jibble pricing third-parties).

Raw research artifacts: `consolidated-brief-20261016.md`, `jibble-deep-dive-2026-10-16.md`, `hris-agent-ready-rea-20261016.md` (same folder).
