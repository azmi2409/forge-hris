# Payroll engine (component 5)

The payroll engine computes statutory-accurate gross → net pay for Malaysia
and Indonesia, behind a two-phase preview → submit gate. It is split into two
halves that never cross:

- **`payroll/`** — the pure calculation package (stdlib only, no storage):
  statutory rule packs, the gross → net engine, item-preset catalog,
  payslip HTML, bank files, and statutory exports. It only computes.
- **`store/payroll.go`** (+ memory/postgres adapters, migration 006) — runs,
  pay lines, item presets, sync items, and preview payloads. The lifecycle
  (draft → approved → locked, cancelled as a terminal side state) lives here.

All money is `int64` minor units everywhere: **sen** for MYR, **whole rupiah**
for IDR (rupiah has no minor unit, so 1 unit = Rp 1).

## Lifecycle

```
draft → approved → locked
  │         │
  └→ cancelled (terminal, from draft or approved)
```

- Only **draft** runs accept pay-line edits (`UpsertPayLine`,
  `AddSyncItems` → `RUN_NOT_DRAFT` otherwise).
- **approved** runs are payroll-committed: payslips issued, webhooks fanned
  out. **locked** is the final books-closed state.
- **cancelled** is terminal; computed pay lines are kept for audit.
- `(organization_id, period)` is unique — one run per YYYY-MM period.
- Every transition is optimistic-concurrency checked (`version`) and audited.

## Rule packs

Statutory behavior is derived from per-item preset flags + worker status,
never hard-coded per run. `payroll.RulePackVersion(country)` identifies the
pack (`my-2026-10`, `id-2026-10`); **bump it when rates change** (SKBBK
phases, the yearly JP cap, Budget relief changes).

### Malaysia (`MY`)

| Item | Implemented | Basis in code |
|---|---|---|
| EPF (KWSP) | 11%/13%/12% employee/employer split by age & citizenship; Third Schedule sections A/C/E/F; foreign-worker 2%/2% regime; whole-RM rounding approximation | `payroll/my.go` |
| SOCSO (PERKESO Act 4) | Banded contribution table, RM6,000 ceiling | `payroll/my.go` |
| SKBBK (Lindung 24 Jam) | Employee-only Phase 1 0.75% (2026-06-01–2028-05-31), RM6,000 ceiling; mandatory for foreign workers, opt-in for locals; folded into `SOCSOEmployee` | `payroll/my.go` |
| EIS (Act 800) | 0.2% + 0.2%, RM6,000 ceiling; foreign workers and age ≥60 excluded | `payroll/my.go` |
| PCB (LHDN) | Simplified MTD: annualize, apply reliefs, ÷12 | `payroll/my.go` |
| HRDF (HRD Corp) | 1% employer (10+ Malaysian employees); 0.5% voluntary (5–9) | `payroll/my.go` |

### Indonesia (`ID`)

| Item | Implemented | Basis in code |
|---|---|---|
| BPJS Kesehatan PPU | 5% (4% employer + 1% employee), Rp12,000,000/month max wage | `payroll/id.go` |
| JHT | 5.7% (3.7 employer + 2.0 employee) | `payroll/id.go` |
| JKK | Risk classes I–V: 0.24/0.54/0.89/1.27/1.74%, employer-only | `payroll/id.go` |
| JKM | 0.3% employer-only | `payroll/id.go` |
| JP | 3% (2 employer + 1 employee), cap Rp11,086,300/month (2026 value — changes yearly) | `payroll/id.go` |
| PPh 21 | Monthly TER tables, categories A/B/C by PTKP status | `payroll/id.go` |

### Item presets

`payroll.SeedPresets()` is the global PayrollPanda-style catalog: each preset
carries `SubjectTo*` flags that decide which statutory wage bases an addition
feeds, plus `ReducesContributionWages` for deductions (e.g. unpaid leave)
that shrink every contribution-wage base. Orgs can add their own presets
(codes must not collide with seed codes); system presets are immutable.
Unknown `PTKPStatus` defaults to TER category A with a warning.

## ⚠️ Verification warning

**The rule packs were researched against public sources as of 2026-10 and
MUST be re-verified against LHDN / KWSP / PERKESO (MY) and BPJS / DJP (ID)
before any real payroll run.** Statutory correctness is legal liability, not
just a bug. The full known-approximation list:

1. SOCSO reconstructed bands — 19/64 bands verbatim from PERKESO's *Jadual
   Caruman Baharu Merangkumi SKBBK*; rest use documented
   round-half-up-to-5sen(rate×midpoint). Source: perkeso.gov.my Lindung 24
   Jam PDF.
2. EPF whole-RM rounding — approximation of KWSP Third Schedule banded
   amounts; verify before production.
3. EPF legacy pre-1998 foreign members not modeled (mapped to 2%/2%).
4. PCB formula is a simplified annualize/reliefs/÷12 — does not replicate
   LHDN's accumulated-remuneration MTD categories; SKBBK relief treatment
   within the RM350 cap unverified (Budget 2027 adds separate RM150 Lindung
   relief; brackets change for YA2027).
5. EPF top-up PCB relief effect below RM4,000 cap not reflected.
6. JKK PP 49/2023 recomposition (0.14% moved to JKP for JKP-registered
   employers) — implemented full rates per PP 6/2025; verify which applies.
7. Foreign-worker SOCSO category — engine uses pure age rule; PERKESO 2019
   said EI-only for foreigners but a 2025 source says under-60 foreigners
   moved to First Category (Jul 2024); verify.
8. TER tables transcribed from a public copy of the PP 58/2023 appendix
   (cross-checked vs online-pajak.com and DJP PMK 168/2023 slides); verify
   against the official PP 58/2023 PDF.
9. JP cap Rp11,086,300 = 2026 value (changes yearly — bump RulePackVersion).
10. SKBBK Phase 2/3 — table encodes Phase 1 (0.75% to 2028-05-31); rates step
    to 1.00%/1.25% after.
11. Bank/authority file layouts (Maybank template, e-Data PCB txt, i-Akaun
    CSV) — verify against current specs; never file without checking.

## Bank files

PayrollPanda parity: "bank integration" is **file generation for manual
bank-portal upload**, not bank API debits.

- `RenderPipeDelimited(orgCode, creditingDate, credits)` — generic
  pipe-delimited bulk credit file (`payroll/bankfile.go`). `creditingDate`
  is YYYYMMDD; amounts are integer minor units; sequence starts at 1.
- `RenderMaybank(orgCode, creditingDate, credits)` — Maybank-style bulk
  salary-credit file. **⚠ TEMPLATE — NOT the actual Maybank specification.**
  Field order, header semantics, amount formatting (sen vs RM), and trailer
  requirements must be verified against Maybank's current bulk-credit /
  salary-crediting file spec (and the organisation code issued by the bank)
  before use. **Never submit a file generated from this template to a bank
  without that verification.**

Employees missing bank details are skipped from the bank file and flagged
with the `MISSING_BANK_DETAILS` anomaly at preview time.

## Statutory exports

All layouts are functional placeholders marked **VERIFY** against the
authority's current upload spec before filing — see VERIFY list item 11.

| Export | Function | VERIFY status |
|---|---|---|
| EPF i-Akaun CSV | `RenderEPFCsv` — one row per employee | column order, header requirements, amount format |
| e-Data PCB txt | `RenderPCBDataTxt` — monthly PCB data file | record layout, field widths, amount format |
| Borang A summary | `RenderBorangASummary` — plain-text EPF Form A totals + per-employee breakdown | summary use only; confirm against official form |
| Payslip HTML | `RenderPayslipHTML` — clean, printable, no-JS payslip with inline CSS; all dynamic content HTML-escaped | display only (not a filing) |

## Two-phase preview → submit gate

Money movement is never single-step (REA §5.6). Both the REST surface and
the MCP surface implement the same gate; the stores are the shared source of
truth.

1. **Draft** — create the run for a YYYY-MM period (`draft`).
2. **Calculate / preview** — dry-run `payroll.Calculate` over the employee
   inputs; `DiffRunPreview` diffs the results against the previous run's
   pay lines with anomaly flags (`NET_SWING_GT_20`, `NEW_EMPLOYEE`,
   `MISSING_BANK_DETAILS`, `NO_PRIOR_RUN`); the computed pay lines are
   persisted on the draft run; a preview payload (totals + anomalies) is
   saved with a **30-minute expiry**. REST additionally auto-folds approved
   unpaid leave and staged sync items into the calculation; the MCP
   `hris.preview_payroll` tool accepts `unpaid_leave_days` explicitly.
3. **Submit** — requires the live `preview_id` plus the run's current
   `version`. The gate replays, in order:
   - run must be `draft` → else `409 RUN_NOT_DRAFT`;
   - preview must exist and be unexpired → else `PREVIEW_EXPIRED` ("preview
     expired or unknown");
   - preview must belong to this run → else `PREVIEW_MISMATCH`;
   - `draft → approved` (version-checked; `409 VERSION_CONFLICT` on stale);
   - enqueue `payroll.submitted` `{run_id, period}`;
   - enqueue one `payslip.issued` `{run_id, employee_id}` per pay line;
   - audit `payroll.submitted` on the `payroll_run` resource.

The MCP `hris.submit_payroll` tool is **TierNever**: the first call returns
the preview (totals + anomaly count) with `approval_required:true`; the
follow-up must carry the single-use resume token **and** `confirmed:true`.
Cancelling a run (`cancel_payroll_run`, TierConfirm) moves
draft/approved → cancelled and emits `payroll.cancelled`; lines are kept for
audit.

## Webhook events

`payroll.drafted`, `payroll.calculated`, `payroll.submitted`,
`payroll.cancelled`, and per-payslip `payslip.issued` — all in the
`store.WebhookEvent*` taxonomy and subscribable (including via `*`).

## Scopes / RBAC matrix

| Capability | Scope | Min role |
|---|---|---|
| List / get runs, read payslips, list pay schedules | `payroll:read` | member* |
| Draft runs, calculate previews | `payroll:preview` | — (scope only) |
| Submit / cancel runs | `payroll:submit` | **admin** |

\* Members hold `payroll:read` only when explicitly granted (it is not in
the default member scope set); a member may fetch **only their own**
payslip. Owners and admins hold every scope. `payroll:submit` may only be
granted by an owner (service-account rule, component 1).
