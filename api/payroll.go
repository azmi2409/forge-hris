// Package api — payroll (component 5): payroll runs with the
// draft -> approved -> locked lifecycle, item presets, sync items, the
// preview -> submit two-phase gate, payslips, bank files, and statutory
// exports.
//
// All money is int64 minor units (sen for MYR, whole rupiah for IDR)
// everywhere. The pure calculation lives in the payroll package (engine,
// rule packs, exports); these handlers only translate HTTP into store
// calls plus payroll.Calculate. REST and MCP share the same store calls;
// the submit-gate algorithm below mirrors the hris.submit_payroll MCP
// tool exactly.
//
// The payroll sentinels are mapped by the shared mapError in server.go:
// ErrRunNotDraft -> 409 RUN_NOT_DRAFT, ErrIllegalRunTransition -> 409
// ILLEGAL_TRANSITION, ErrSystemPresetImmutable -> 409
// SYSTEM_PRESET_IMMUTABLE.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/payroll"
	"github.com/azmi2409/forge-hris/store"
)

// previewTTL bounds how long a payroll preview stays submittable.
const previewTTL = 30 * time.Minute

// payrollRoutes registers the component 5 routes. Called from routes().
func (s *Server) payrollRoutes() {
	m := s.mux
	base := "/api/v1/organizations/{orgID}"

	m.HandleFunc("POST "+base+"/payroll-runs",
		s.withAudit("payroll.drafted", "payroll_run", s.createPayrollRun))
	m.HandleFunc("GET "+base+"/payroll-runs", s.handleListPayrollRuns)
	m.HandleFunc("GET "+base+"/payroll-runs/{id}", s.handleGetPayrollRun)
	m.HandleFunc("POST "+base+"/payroll-runs/{id}/calculate",
		s.withAudit("payroll.calculated", "payroll_run", s.calculatePayrollRun))
	m.HandleFunc("POST "+base+"/payroll-runs/{id}/preview", s.handlePreviewPayrollRun)
	m.HandleFunc("POST "+base+"/payroll-runs/{id}/submit",
		s.withAudit("payroll.submitted", "payroll_run", s.submitPayrollRun))
	m.HandleFunc("POST "+base+"/payroll-runs/{id}/lock",
		s.withAudit("payroll.locked", "payroll_run", s.lockPayrollRun))
	m.HandleFunc("POST "+base+"/payroll-runs/{id}/cancel",
		s.withAudit("payroll.cancelled", "payroll_run", s.cancelPayrollRun))
	m.HandleFunc("POST "+base+"/payroll-runs/{id}/sync-items",
		s.withAudit("payroll.sync_items_added", "payroll_run", s.addPayrollSyncItems))

	m.HandleFunc("GET "+base+"/payroll-runs/{id}/payslips/{employeeID}", s.handleGetPayslip)
	m.HandleFunc("GET "+base+"/payroll-runs/{id}/bank-file", s.handlePayrollBankFile)
	m.HandleFunc("GET "+base+"/payroll-runs/{id}/exports/epf-csv", s.handlePayrollEPFExport)
	m.HandleFunc("GET "+base+"/payroll-runs/{id}/exports/pcb-txt", s.handlePayrollPCBExport)
	m.HandleFunc("GET "+base+"/payroll-runs/{id}/exports/borang-a", s.handlePayrollBorangAExport)

	m.HandleFunc("POST "+base+"/payroll-presets",
		s.withAudit("payroll_preset.created", "payroll_preset", s.createPayrollPreset))
	m.HandleFunc("GET "+base+"/payroll-presets", s.handleListPayrollPresets)
	m.HandleFunc("PATCH "+base+"/payroll-presets/{id}",
		s.withAudit("payroll_preset.updated", "payroll_preset", s.patchPayrollPreset))
}

// payrollStoreOf returns the store's PayrollStore facet. Both bundled store
// implementations provide it; the facet is embedded in Store.
func (s *Server) payrollStoreOf(w http.ResponseWriter, r *http.Request) (store.PayrollStore, bool) {
	ps, ok := s.store.(store.PayrollStore)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "payroll store not available")
		return nil, false
	}
	return ps, true
}

// requirePayrollRoleOrScope is requireRoleOrScope with a member carve-out.
// Members hold payroll:read for own-payslip self-service only (enforced by
// the payslip handler's self-check), so on every other payroll endpoint the
// scope leg must not admit member PATs. Service accounts have no role and
// pass on the scope alone, preserving least-privilege agent access.
func requirePayrollRoleOrScope(w http.ResponseWriter, r *http.Request, ai *AuthInfo, min store.Role, scope, what string) bool {
	if requireRole(min, ai) {
		return true
	}
	if ai.Kind == store.CredentialServiceAccount && requireScope(scope, ai) {
		return true
	}
	writeError(w, r, http.StatusForbidden, "FORBIDDEN",
		"requires "+string(min)+" role or higher, or scope "+scope+what)
	return false
}

// requireDraftRun loads the run and rejects non-draft runs with 409
// RUN_NOT_DRAFT, so every run-mutating endpoint shares one error shape.
func (s *Server) requireDraftRun(w http.ResponseWriter, r *http.Request, ps store.PayrollStore, orgID, runID string) (store.PayrollRun, bool) {
	run, err := ps.GetPayrollRun(r.Context(), orgID, runID)
	if !mapError(w, r, err) {
		return store.PayrollRun{}, false
	}
	if run.Status != store.PayrollRunDraft {
		mapError(w, r, fmt.Errorf("%w: payroll run %s is %s", store.ErrRunNotDraft, runID, run.Status))
		return store.PayrollRun{}, false
	}
	return run, true
}

// --- payroll runs ---

type createPayrollRunBody struct {
	Period      string `json:"period"`       // YYYY-MM
	CountryCode string `json:"country_code"` // MY | ID
}

func (s *Server) createPayrollRun(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollPreview, " to draft payroll runs") {
		return zero, false
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req createPayrollRunBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if strings.TrimSpace(req.Period) == "" || (req.CountryCode != "MY" && req.CountryCode != "ID") {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"period (YYYY-MM) and country_code (MY|ID) are required")
		return zero, false
	}
	run, err := ps.CreatePayrollRun(r.Context(), store.PayrollRun{
		OrganizationID:  orgID,
		Period:          req.Period,
		CountryCode:     req.CountryCode,
		RulePackVersion: payroll.RulePackVersion(req.CountryCode),
	})
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, run)
	s.emitWebhook(r.Context(), orgID, store.WebhookEventPayrollDrafted, run)
	return auditData{ResourceID: run.ID, After: run}, true
}

func (s *Server) handleListPayrollRuns(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollRead, " to read payroll") {
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := store.PayrollRunFilter{Period: q.Get("period")}
	if v := q.Get("status"); v != "" {
		filter.Status = store.PayrollRunStatus(v)
		if !store.ValidPayrollRunStatus(filter.Status) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "status: unknown payroll run status")
			return
		}
	}
	runs, next, err := ps.ListPayrollRuns(r.Context(), orgID, filter, page)
	if !mapError(w, r, err) {
		return
	}
	if runs == nil {
		runs = []store.PayrollRun{}
	}
	writePage(w, runs, next)
}

func (s *Server) handleGetPayrollRun(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollRead, " to read payroll") {
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	run, err := ps.GetPayrollRun(r.Context(), orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// --- calculate / preview inputs ---

type payrollItemInput struct {
	PresetCode  string `json:"preset_code"`
	AmountMinor int64  `json:"amount_minor"`
}

type calculateEmployeeInput struct {
	EmployeeID       string             `json:"employee_id"`
	BaseSalaryMinor  int64              `json:"base_salary_minor"`
	Items            []payrollItemInput `json:"items"`
	AgeYears         int                `json:"age_years"`
	CitizenMY        bool               `json:"citizen_my"`
	ForeignWorkerMY  bool               `json:"foreign_worker_my"`
	PRMY             bool               `json:"pr_my"`
	SKBBKOptIn       bool               `json:"skbbk_opt_in"`
	EmployerBornePCB bool               `json:"employer_borne_pcb"`
	JKKRiskClass     int                `json:"jkk_risk_class"`
	PTKPStatus       string             `json:"ptkp_status"`
	BankAccount      string             `json:"bank_account"`
	BankCode         string             `json:"bank_code"`
}

type calculateRequest struct {
	Employees []calculateEmployeeInput `json:"employees"`
}

// buildPayInputs validates the calculate/preview body and assembles the
// engine inputs: caller items plus staged sync items, with approved unpaid
// leave auto-folded in (IsUnpaid && overlapping the run month). Daily rate
// is base salary / 26 — a documented simplification (26 working
// days/month), not a statutory rule.
func (s *Server) buildPayInputs(w http.ResponseWriter, r *http.Request, ps store.PayrollStore, orgID string, run store.PayrollRun, req calculateRequest) ([]payroll.PayInput, []payroll.Preset, bool) {
	ctx := r.Context()

	var presets []store.PayrollItemPreset
	cursor := ""
	for {
		page, next, err := ps.ListPayrollPresets(ctx, orgID, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return nil, nil, false
		}
		presets = append(presets, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	presetByCode := make(map[string]payroll.Preset, len(presets))
	plist := make([]payroll.Preset, 0, len(presets))
	for _, p := range presets {
		pp := store.PresetToPayroll(p)
		presetByCode[p.Code] = pp
		plist = append(plist, pp)
	}

	// Staged sync items, grouped by employee.
	syncByEmp := map[string][]payroll.PayItemInput{}
	cursor = ""
	for {
		items, next, err := ps.ListSyncItems(ctx, orgID, run.ID, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return nil, nil, false
		}
		for _, it := range items {
			if strings.TrimSpace(it.PresetCode) == "" {
				writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
					fmt.Sprintf("sync item %s: preset_code is required", it.ID))
				return nil, nil, false
			}
			if _, ok := presetByCode[it.PresetCode]; !ok {
				writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
					fmt.Sprintf("sync item %s: unknown preset code %q", it.ID, it.PresetCode))
				return nil, nil, false
			}
			syncByEmp[it.EmployeeID] = append(syncByEmp[it.EmployeeID], payroll.PayItemInput{
				PresetCode: it.PresetCode, AmountMinor: it.AmountMinor,
			})
		}
		if next == "" {
			break
		}
		cursor = next
	}

	// Approved unpaid leave overlapping the run month, per employee.
	unpaidByEmp := map[string]float64{}
	ls, ok := s.store.(store.LeaveStore)
	if ok {
		var approved []store.TimeOffRequest
		cursor = ""
		for {
			page, next, err := ls.ListTimeOffRequests(ctx, orgID,
				store.TimeOffRequestFilter{Status: store.TimeOffApproved},
				store.Page{Limit: 1000, Cursor: cursor})
			if !mapError(w, r, err) {
				return nil, nil, false
			}
			approved = append(approved, page...)
			if next == "" {
				break
			}
			cursor = next
		}
		unpaidByEmp = unpaidLeaveDaysForPeriod(approved, run.Period)
	}

	inputs := make([]payroll.PayInput, 0, len(req.Employees))
	for i, e := range req.Employees {
		if strings.TrimSpace(e.EmployeeID) == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("employees[%d].employee_id: required", i))
			return nil, nil, false
		}
		emp, err := s.store.GetEmployee(ctx, orgID, e.EmployeeID)
		if !mapError(w, r, err) {
			return nil, nil, false
		}
		if emp.Status != store.InvitationJoined {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("employees[%d]: employee %s is not joined (status %s)", i, e.EmployeeID, emp.Status))
			return nil, nil, false
		}
		items := make([]payroll.PayItemInput, 0, len(e.Items)+len(syncByEmp[e.EmployeeID]))
		for j, it := range e.Items {
			if _, ok := presetByCode[it.PresetCode]; !ok {
				writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
					fmt.Sprintf("employees[%d].items[%d]: unknown preset code %q", i, j, it.PresetCode))
				return nil, nil, false
			}
			items = append(items, payroll.PayItemInput{PresetCode: it.PresetCode, AmountMinor: it.AmountMinor})
		}
		items = append(items, syncByEmp[e.EmployeeID]...)
		inputs = append(inputs, payroll.PayInput{
			EmployeeID:       e.EmployeeID,
			CountryCode:      run.CountryCode,
			BaseSalaryMinor:  e.BaseSalaryMinor,
			Items:            items,
			UnpaidLeaveDays:  unpaidByEmp[e.EmployeeID],
			DailyRateMinor:   e.BaseSalaryMinor / 26,
			AgeYears:         e.AgeYears,
			CitizenMY:        e.CitizenMY,
			ForeignWorkerMY:  e.ForeignWorkerMY,
			PRMY:             e.PRMY,
			SKBBKOptIn:       e.SKBBKOptIn,
			EmployerBornePCB: e.EmployerBornePCB,
			JKKRiskClass:     e.JKKRiskClass,
			PTKPStatus:       e.PTKPStatus,
			BankAccount:      e.BankAccount,
			BankCode:         e.BankCode,
		})
	}
	return inputs, plist, true
}

// unpaidLeaveDaysForPeriod sums, per employee, the calendar days of approved
// unpaid leave requests overlapping the run's YYYY-MM month.
func unpaidLeaveDaysForPeriod(reqs []store.TimeOffRequest, period string) map[string]float64 {
	monthStart, err := time.Parse("2006-01-02", period+"-01")
	if err != nil {
		return nil
	}
	monthEnd := monthStart.AddDate(0, 1, -1)
	out := map[string]float64{}
	for _, lr := range reqs {
		if !lr.IsUnpaid {
			continue
		}
		start, err1 := time.Parse("2006-01-02", lr.StartDate)
		end, err2 := time.Parse("2006-01-02", lr.EndDate)
		if err1 != nil || err2 != nil || end.Before(start) {
			continue
		}
		s, e := start, end
		if s.Before(monthStart) {
			s = monthStart
		}
		if e.After(monthEnd) {
			e = monthEnd
		}
		if e.Before(s) {
			continue
		}
		out[lr.EmployeeID] += float64(int(e.Sub(s).Hours()/24) + 1)
	}
	return out
}

// persistPayLines upserts one pay line per calculated result on the draft
// run. Re-running is safe: an existing line is updated via optimistic
// concurrency (its stored version is carried onto the upsert). Shared by
// calculate and preview so both surfaces persist identical lines.
func (s *Server) persistPayLines(ctx context.Context, ps store.PayrollStore, orgID, runID string, inputs []payroll.PayInput, results []payroll.PayResult) ([]store.EmployeePayLine, error) {
	bankByEmp := make(map[string]payroll.PayInput, len(inputs))
	for _, in := range inputs {
		bankByEmp[in.EmployeeID] = in
	}
	lines := make([]store.EmployeePayLine, 0, len(results))
	for _, res := range results {
		in := bankByEmp[res.EmployeeID]
		line := store.EmployeePayLine{
			OrganizationID:  orgID,
			RunID:           runID,
			EmployeeID:      res.EmployeeID,
			GrossMinor:      res.GrossMinor,
			AdditionsMinor:  res.AdditionsMinor,
			DeductionsMinor: res.DeductionsMinor,
			NetMinor:        res.NetMinor,
			Items:           res.Items,
			Statutory:       res.Splits,
			BankAccount:     in.BankAccount,
			BankCode:        in.BankCode,
		}
		if cur, gerr := ps.GetPayLine(ctx, orgID, runID, res.EmployeeID); gerr == nil {
			line.Version = cur.Version
		} else if !errors.Is(gerr, store.ErrNotFound) {
			return nil, gerr
		}
		saved, err := ps.UpsertPayLine(ctx, line)
		if err != nil {
			return nil, err
		}
		lines = append(lines, saved)
	}
	return lines, nil
}

// calculatePayrollRun runs payroll.Calculate on the draft run and upserts
// one pay line per employee. Re-running is safe: existing lines are
// updated via optimistic concurrency.
func (s *Server) calculatePayrollRun(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollPreview, " to calculate payroll") {
		return zero, false
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return zero, false
	}
	run, ok := s.requireDraftRun(w, r, ps, orgID, r.PathValue("id"))
	if !ok {
		return zero, false
	}
	var req calculateRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	inputs, presets, ok := s.buildPayInputs(w, r, ps, orgID, run, req)
	if !ok {
		return zero, false
	}
	results, err := payroll.Calculate(run.CountryCode, presets, inputs)
	if !mapError(w, r, err) {
		return zero, false
	}
	ctx := r.Context()
	lines, err := s.persistPayLines(ctx, ps, orgID, run.ID, inputs, results)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": run.ID, "lines": lines})
	s.emitWebhook(ctx, orgID, store.WebhookEventPayrollCalculated, map[string]any{
		"run_id": run.ID, "period": run.Period, "employee_count": len(lines),
	})
	return auditData{ResourceID: run.ID, After: lines}, true
}

// --- preview -> submit gate ---

// previewTotals aggregates the run totals stored in the preview payload.
type previewTotals struct {
	EmployeeCount int   `json:"employee_count"`
	GrossMinor    int64 `json:"gross_minor"`
	NetMinor      int64 `json:"net_minor"`
}

func totalsOf(results []payroll.PayResult) previewTotals {
	t := previewTotals{EmployeeCount: len(results)}
	for _, r := range results {
		t.GrossMinor += r.GrossMinor
		t.NetMinor += r.NetMinor
	}
	return t
}

// previousRunResults returns the latest non-cancelled run before run.Period
// as PayResults for the diff, or nil when there is no prior run (every
// employee then gets NO_PRIOR_RUN). Only net pay and the missing-bank
// warning survive the round trip — the diff only needs those.
func (s *Server) previousRunResults(w http.ResponseWriter, r *http.Request, ps store.PayrollStore, orgID string, run store.PayrollRun) ([]payroll.PayResult, bool) {
	ctx := r.Context()
	var prev *store.PayrollRun
	cursor := ""
	for {
		runs, next, err := ps.ListPayrollRuns(ctx, orgID, store.PayrollRunFilter{}, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return nil, false
		}
		for i := range runs {
			if runs[i].Status == store.PayrollRunCancelled || runs[i].Period >= run.Period {
				continue
			}
			if prev == nil || runs[i].Period > prev.Period {
				c := runs[i]
				prev = &c
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if prev == nil {
		return nil, true
	}
	var out []payroll.PayResult
	cursor = ""
	for {
		lines, next, err := ps.ListPayLines(ctx, orgID, prev.ID, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return nil, false
		}
		for _, l := range lines {
			pr := payroll.PayResult{EmployeeID: l.EmployeeID, NetMinor: l.NetMinor}
			if strings.TrimSpace(l.BankAccount) == "" {
				pr.Warnings = []string{payroll.WarnMissingBankDetails}
			}
			out = append(out, pr)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, true
}

// handlePreviewPayrollRun is the dry-run half of the two-phase gate: it
// computes the run, persists the lines on the draft (identical semantics to
// the MCP preview — both surfaces share persistPayLines, so REST and MCP
// never drift), diffs against the previous run, and stores the 30-minute
// preview payload that submit requires.
func (s *Server) handlePreviewPayrollRun(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollPreview, " to preview payroll") {
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	run, ok := s.requireDraftRun(w, r, ps, orgID, r.PathValue("id"))
	if !ok {
		return
	}
	var req calculateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	inputs, presets, ok := s.buildPayInputs(w, r, ps, orgID, run, req)
	if !ok {
		return
	}
	results, err := payroll.Calculate(run.CountryCode, presets, inputs)
	if !mapError(w, r, err) {
		return
	}
	ctx := r.Context()
	if _, err := s.persistPayLines(ctx, ps, orgID, run.ID, inputs, results); !mapError(w, r, err) {
		return
	}
	prev, ok := s.previousRunResults(w, r, ps, orgID, run)
	if !ok {
		return
	}
	employees := payroll.DiffRunPreview(prev, results)
	totals := totalsOf(results)
	payload, err := json.Marshal(map[string]any{"employees": employees, "totals": totals})
	if !mapError(w, r, err) {
		return
	}
	expiresAt := time.Now().UTC().Add(previewTTL)
	saved, err := ps.SavePayrollPreview(ctx, store.PayrollPreview{
		OrganizationID: orgID,
		RunID:          run.ID,
		Payload:        payload,
		ExpiresAt:      expiresAt,
	})
	if !mapError(w, r, err) {
		return
	}
	s.emitWebhook(ctx, orgID, store.WebhookEventPayrollCalculated, map[string]any{
		"run_id": run.ID, "period": run.Period, "employee_count": len(results), "via": "rest",
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"preview_id": saved.ID,
		"expires_at": saved.ExpiresAt,
		"employees":  employees,
		"totals":     totals,
	})
}

type submitPayrollBody struct {
	PreviewID string `json:"preview_id"`
	Version   int64  `json:"version"`
}

// submitPayrollRun is the second half of the two-phase gate. The algorithm
// mirrors the hris.submit_payroll MCP tool exactly: the run must be draft,
// the preview must exist, be unexpired, and belong to this run; only then
// does draft -> approved happen. Idempotent via Idempotency-Key.
func (s *Server) submitPayrollRun(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	// Submit needs payroll:submit scope or admin role — never the preview
	// scope: previewing must not imply the right to approve.
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopePayrollSubmit, " to submit payroll") {
		return zero, false
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return zero, false
	}
	run, ok := s.requireDraftRun(w, r, ps, orgID, r.PathValue("id"))
	if !ok {
		return zero, false
	}
	var req submitPayrollBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if strings.TrimSpace(req.PreviewID) == "" {
		writeError(w, r, http.StatusConflict, "PREVIEW_REQUIRED",
			"submit requires a preview: POST /payroll-runs/{id}/preview first")
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	ctx := r.Context()
	preview, err := ps.GetPayrollPreview(ctx, orgID, req.PreviewID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Unknown or expired (the store reports both as
			// ErrNotFound): the caller must generate a fresh preview.
			writeError(w, r, http.StatusConflict, "PREVIEW_STALE",
				"preview not found or expired; generate a fresh preview")
			return zero, false
		}
		if !mapError(w, r, err) {
			return zero, false
		}
	}
	if preview.RunID != run.ID {
		writeError(w, r, http.StatusConflict, "PREVIEW_STALE",
			"preview belongs to a different payroll run")
		return zero, false
	}
	approved, err := ps.TransitionPayrollRun(ctx, orgID, run.ID, req.Version, store.PayrollRunApproved, ai.CredentialID)
	if !mapError(w, r, err) {
		return zero, false
	}
	var lines []store.EmployeePayLine
	cursor := ""
	for {
		page, next, err := ps.ListPayLines(ctx, orgID, run.ID, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return zero, false
		}
		lines = append(lines, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	writeJSON(w, http.StatusOK, approved)
	s.emitWebhook(ctx, orgID, store.WebhookEventPayrollSubmitted, approved)
	for _, l := range lines {
		s.emitWebhook(ctx, orgID, store.WebhookEventPayslipIssued, l)
	}
	return auditData{ResourceID: approved.ID, Before: run, After: approved}, true
}

type runVersionBody struct {
	Version int64 `json:"version"`
}

// lockPayrollRun moves approved -> locked. The store rejects every other
// transition with 409 ILLEGAL_TRANSITION.
func (s *Server) lockPayrollRun(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopePayrollSubmit, " to lock payroll") {
		return zero, false
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req runVersionBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	before, err := ps.GetPayrollRun(r.Context(), orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return zero, false
	}
	locked, err := ps.TransitionPayrollRun(r.Context(), orgID, before.ID, req.Version, store.PayrollRunLocked, ai.CredentialID)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, locked)
	return auditData{ResourceID: locked.ID, Before: before, After: locked}, true
}

// cancelPayrollRun moves draft|approved -> cancelled. The store rejects
// every other transition with 409 ILLEGAL_TRANSITION.
func (s *Server) cancelPayrollRun(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopePayrollSubmit, " to cancel payroll") {
		return zero, false
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req runVersionBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	before, err := ps.GetPayrollRun(r.Context(), orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return zero, false
	}
	cancelled, err := ps.TransitionPayrollRun(r.Context(), orgID, before.ID, req.Version, store.PayrollRunCancelled, ai.CredentialID)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, cancelled)
	return auditData{ResourceID: cancelled.ID, Before: before, After: cancelled}, true
}

// --- sync items ---

type syncItemInput struct {
	EmployeeID  string `json:"employee_id"`
	Source      string `json:"source"`
	ExternalID  string `json:"external_id"`
	Label       string `json:"label"`
	Kind        string `json:"kind"` // addition | deduction
	PresetCode  string `json:"preset_code"`
	AmountMinor int64  `json:"amount_minor"`
}

type addSyncItemsBody struct {
	Items []syncItemInput `json:"items"`
}

// addPayrollSyncItems stages externally-ingested items on a draft run.
// They are folded into the next calculate call.
func (s *Server) addPayrollSyncItems(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollPreview, " to add sync items") {
		return zero, false
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return zero, false
	}
	run, ok := s.requireDraftRun(w, r, ps, orgID, r.PathValue("id"))
	if !ok {
		return zero, false
	}
	var req addSyncItemsBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	ctx := r.Context()
	presetCodes := map[string]bool{}
	cursor := ""
	for {
		page, next, err := ps.ListPayrollPresets(ctx, orgID, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return zero, false
		}
		for _, p := range page {
			presetCodes[p.Code] = true
		}
		if next == "" {
			break
		}
		cursor = next
	}
	items := make([]store.SyncItem, 0, len(req.Items))
	for i, it := range req.Items {
		if strings.TrimSpace(it.EmployeeID) == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("items[%d].employee_id: required", i))
			return zero, false
		}
		if it.Kind != "addition" && it.Kind != "deduction" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("items[%d].kind: must be addition or deduction", i))
			return zero, false
		}
		if strings.TrimSpace(it.PresetCode) == "" || !presetCodes[it.PresetCode] {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("items[%d].preset_code: unknown preset code %q", i, it.PresetCode))
			return zero, false
		}
		if emp, err := s.store.GetEmployee(ctx, orgID, it.EmployeeID); err != nil {
			if !mapError(w, r, err) {
				return zero, false
			}
		} else if emp.Status != store.InvitationJoined {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				fmt.Sprintf("items[%d]: employee %s is not joined (status %s)", i, it.EmployeeID, emp.Status))
			return zero, false
		}
		items = append(items, store.SyncItem{
			OrganizationID: orgID,
			RunID:          run.ID,
			EmployeeID:     it.EmployeeID,
			Source:         it.Source,
			ExternalID:     it.ExternalID,
			Label:          it.Label,
			Kind:           payroll.PresetKind(it.Kind),
			PresetCode:     it.PresetCode,
			AmountMinor:    it.AmountMinor,
		})
	}
	if err := ps.AddSyncItems(ctx, orgID, run.ID, items); !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": run.ID, "added": len(items)})
	return auditData{ResourceID: run.ID, After: map[string]any{"added": len(items)}}, true
}

// --- payslips ---

// handleGetPayslip returns one employee's pay line as JSON, or as rendered
// HTML with ?format=html. Members may only read their own payslip (same
// self-check as the employee GET handler); everyone else needs
// payroll:read.
func (s *Server) handleGetPayslip(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireRoleOrScope(w, r, ai, store.RoleMember, store.ScopePayrollRead, " to read payslips") {
		return
	}
	employeeID := r.PathValue("employeeID")
	if effectiveRole(ai) == store.RoleMember &&
		(ai.EmployeeID == nil || *ai.EmployeeID != employeeID) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN",
			"members may only read their own payslip")
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	run, err := ps.GetPayrollRun(ctx, orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return
	}
	line, err := ps.GetPayLine(ctx, orgID, run.ID, employeeID)
	if !mapError(w, r, err) {
		return
	}
	if r.URL.Query().Get("format") == "html" {
		emp, err := s.store.GetEmployee(ctx, orgID, employeeID)
		if !mapError(w, r, err) {
			return
		}
		org, err := s.store.GetOrganization(ctx, orgID)
		if !mapError(w, r, err) {
			return
		}
		currency := "MYR"
		if run.CountryCode == "ID" {
			currency = "IDR"
		}
		data := payroll.PayslipData{
			CompanyName:     org.Name,
			EmployeeName:    strings.TrimSpace(emp.FirstName + " " + emp.LastName),
			EmployeeNumber:  emp.EmployeeNumber,
			Period:          run.Period,
			Currency:        currency,
			Items:           line.Items,
			GrossMinor:      line.GrossMinor,
			DeductionsMinor: line.DeductionsMinor,
			NetMinor:        line.NetMinor,
			Splits:          line.Statutory,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(payroll.RenderPayslipHTML(data)))
		return
	}
	writeJSON(w, http.StatusOK, line)
}

// --- bank files and statutory exports ---

// exportRun enforces the bank-file/export gates shared by every download
// endpoint: the run must be approved or locked. Callers add their own
// country gate on top.
func (s *Server) exportRun(w http.ResponseWriter, r *http.Request, ps store.PayrollStore, orgID, runID string) (store.PayrollRun, bool) {
	run, err := ps.GetPayrollRun(r.Context(), orgID, runID)
	if !mapError(w, r, err) {
		return store.PayrollRun{}, false
	}
	if run.Status != store.PayrollRunApproved && run.Status != store.PayrollRunLocked {
		writeError(w, r, http.StatusConflict, "RUN_NOT_APPROVED",
			"run must be approved or locked before files can be generated")
		return store.PayrollRun{}, false
	}
	return run, true
}

// requireMYRun rejects non-MY runs on the MY-only statutory exports.
func requireMYRun(w http.ResponseWriter, r *http.Request, run store.PayrollRun) bool {
	if run.CountryCode != "MY" {
		writeError(w, r, http.StatusConflict, "WRONG_COUNTRY",
			"this export is only available for MY payroll runs")
		return false
	}
	return true
}

// allPayLines pages through every pay line of a run.
func (s *Server) allPayLines(w http.ResponseWriter, r *http.Request, ps store.PayrollStore, orgID, runID string) ([]store.EmployeePayLine, bool) {
	ctx := r.Context()
	var all []store.EmployeePayLine
	cursor := ""
	for {
		page, next, err := ps.ListPayLines(ctx, orgID, runID, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return nil, false
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return all, true
}

// lastDayOfPeriod renders YYYY-MM as YYYYMMDD of its last day, for bank
// crediting dates.
func lastDayOfPeriod(period string) string {
	start, err := time.Parse("2006-01-02", period+"-01")
	if err != nil {
		return strings.ReplaceAll(period, "-", "")
	}
	return start.AddDate(0, 1, -1).Format("20060102")
}

// handlePayrollBankFile renders the salary-credit file for an approved or
// locked run. Every pay line is included, even when the employee has no
// bank account on file (rendered as an empty account field — fix the
// employee record and regenerate). Amounts are minor units (sen / rupiah).
func (s *Server) handlePayrollBankFile(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollRead, " to generate bank files") {
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	run, ok := s.exportRun(w, r, ps, orgID, r.PathValue("id"))
	if !ok {
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "pipe"
	}
	if format != "pipe" && format != "maybank" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "format: must be pipe or maybank")
		return
	}
	lines, ok := s.allPayLines(w, r, ps, orgID, run.ID)
	if !ok {
		return
	}
	ctx := r.Context()
	org, err := s.store.GetOrganization(ctx, orgID)
	if !mapError(w, r, err) {
		return
	}
	credits := make([]payroll.BankCredit, 0, len(lines))
	for _, l := range lines {
		name, number := l.EmployeeID, ""
		if emp, err := s.store.GetEmployee(ctx, orgID, l.EmployeeID); err == nil {
			name = strings.TrimSpace(emp.FirstName + " " + emp.LastName)
			number = emp.EmployeeNumber
		}
		credits = append(credits, payroll.BankCredit{
			AccountNo:   l.BankAccount,
			AccountName: name,
			AmountMinor: l.NetMinor,
			Reference:   "PAYROLL " + run.Period + " " + number,
		})
	}
	var out string
	if format == "maybank" {
		out = payroll.RenderMaybank(org.Slug, lastDayOfPeriod(run.Period), credits)
	} else {
		out = payroll.RenderPipeDelimited(org.Slug, lastDayOfPeriod(run.Period), credits)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="payroll-%s-%s.txt"`, run.Period, format))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}

// epfWagesMinor re-derives the EPF contribution-wage base from a stored pay
// line: additions flagged SubjectToEPF minus deductions flagged
// ReducesContributionWages, floored at zero — the same rule the engine
// applies (payroll/engine.go baseWages). The two engine-synthesized codes
// keep their documented flags when the catalog lacks them.
func epfWagesMinor(line store.EmployeePayLine, presetByCode map[string]payroll.Preset) int64 {
	var wages int64
	for _, it := range line.Items {
		p, ok := presetByCode[it.PresetCode]
		if !ok {
			switch it.PresetCode {
			case "basic_salary":
				p = payroll.Preset{Kind: payroll.PresetAddition, SubjectToEPF: true}
			case "unpaid_leave_deduction":
				p = payroll.Preset{Kind: payroll.PresetDeduction, ReducesContributionWages: true}
			default:
				continue
			}
		}
		switch p.Kind {
		case payroll.PresetAddition:
			if p.SubjectToEPF {
				wages += it.AmountMinor
			}
		case payroll.PresetDeduction:
			if p.ReducesContributionWages {
				wages -= it.AmountMinor
			}
		}
	}
	if wages < 0 {
		wages = 0
	}
	return wages
}

// presetMapByCode loads the merged preset catalog as a code -> Preset map
// for the export wage-base derivation.
func (s *Server) presetMapByCode(w http.ResponseWriter, r *http.Request, ps store.PayrollStore, orgID string) (map[string]payroll.Preset, bool) {
	ctx := r.Context()
	out := map[string]payroll.Preset{}
	cursor := ""
	for {
		page, next, err := ps.ListPayrollPresets(ctx, orgID, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return nil, false
		}
		for _, p := range page {
			out[p.Code] = store.PresetToPayroll(p)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return out, true
}

func (s *Server) handlePayrollEPFExport(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollRead, " to generate statutory exports") {
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	run, ok := s.exportRun(w, r, ps, orgID, r.PathValue("id"))
	if !ok {
		return
	}
	if !requireMYRun(w, r, run) {
		return
	}
	lines, ok := s.allPayLines(w, r, ps, orgID, run.ID)
	if !ok {
		return
	}
	presetByCode, ok := s.presetMapByCode(w, r, ps, orgID)
	if !ok {
		return
	}
	ctx := r.Context()
	rows := make([]payroll.EPFRow, 0, len(lines))
	for _, l := range lines {
		name := l.EmployeeID
		if emp, err := s.store.GetEmployee(ctx, orgID, l.EmployeeID); err == nil {
			name = strings.TrimSpace(emp.FirstName + " " + emp.LastName)
		}
		rows = append(rows, payroll.EPFRow{
			EmployeeID:    l.EmployeeID,
			EmployeeName:  name,
			WagesMinor:    epfWagesMinor(l, presetByCode),
			EmployeeMinor: l.Statutory.EPFEmployee,
			EmployerMinor: l.Statutory.EPFEmployer,
		})
	}
	out := payroll.RenderEPFCsv(run.Period, rows)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="epf-%s.csv"`, run.Period))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}

func (s *Server) handlePayrollPCBExport(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollRead, " to generate statutory exports") {
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	run, ok := s.exportRun(w, r, ps, orgID, r.PathValue("id"))
	if !ok {
		return
	}
	if !requireMYRun(w, r, run) {
		return
	}
	lines, ok := s.allPayLines(w, r, ps, orgID, run.ID)
	if !ok {
		return
	}
	ctx := r.Context()
	rows := make([]payroll.PCBRow, 0, len(lines))
	for _, l := range lines {
		name := l.EmployeeID
		if emp, err := s.store.GetEmployee(ctx, orgID, l.EmployeeID); err == nil {
			name = strings.TrimSpace(emp.FirstName + " " + emp.LastName)
		}
		// Employees carry no tax-ID field; the filer fills TaxIDNo from
		// their own records before submitting to LHDN.
		rows = append(rows, payroll.PCBRow{
			EmployeeID:   l.EmployeeID,
			EmployeeName: name,
			PCBMinor:     l.Statutory.PCB,
		})
	}
	out := payroll.RenderPCBDataTxt(run.Period, rows)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="pcb-%s.txt"`, run.Period))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}

func (s *Server) handlePayrollBorangAExport(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleManager, store.ScopePayrollRead, " to generate statutory exports") {
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	run, ok := s.exportRun(w, r, ps, orgID, r.PathValue("id"))
	if !ok {
		return
	}
	if !requireMYRun(w, r, run) {
		return
	}
	lines, ok := s.allPayLines(w, r, ps, orgID, run.ID)
	if !ok {
		return
	}
	presetByCode, ok := s.presetMapByCode(w, r, ps, orgID)
	if !ok {
		return
	}
	ctx := r.Context()
	rows := make([]payroll.BorangAEntry, 0, len(lines))
	for _, l := range lines {
		name := l.EmployeeID
		if emp, err := s.store.GetEmployee(ctx, orgID, l.EmployeeID); err == nil {
			name = strings.TrimSpace(emp.FirstName + " " + emp.LastName)
		}
		rows = append(rows, payroll.BorangAEntry{
			EmployeeID:    l.EmployeeID,
			EmployeeName:  name,
			WagesMinor:    epfWagesMinor(l, presetByCode),
			EmployeeMinor: l.Statutory.EPFEmployee,
			EmployerMinor: l.Statutory.EPFEmployer,
		})
	}
	out := payroll.RenderBorangASummary(run.Period, rows)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="borang-a-%s.txt"`, run.Period))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}

// --- item presets ---

type payrollPresetFlags struct {
	SubjectToEPF             bool `json:"subject_to_epf"`
	SubjectToSOCSO           bool `json:"subject_to_socso"`
	SubjectToEIS             bool `json:"subject_to_eis"`
	SubjectToPCB             bool `json:"subject_to_pcb"`
	SubjectToBPJSKes         bool `json:"subject_to_bpjs_kes"`
	SubjectToJHT             bool `json:"subject_to_jht"`
	SubjectToJKK             bool `json:"subject_to_jkk"`
	SubjectToJKM             bool `json:"subject_to_jkm"`
	SubjectToJP              bool `json:"subject_to_jp"`
	SubjectToPPh21           bool `json:"subject_to_pph21"`
	ReducesContributionWages bool `json:"reduces_contribution_wages"`
}

type createPayrollPresetBody struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Kind string `json:"kind"` // addition | deduction | employer_contribution
	payrollPresetFlags
}

func (s *Server) createPayrollPreset(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopePayrollPreview, " to manage payroll presets") {
		return zero, false
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req createPayrollPresetBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	created, err := ps.CreatePayrollPreset(r.Context(), orgID, store.PayrollItemPreset{
		OrganizationID:           orgID,
		Code:                     req.Code,
		Name:                     req.Name,
		Kind:                     payroll.PresetKind(req.Kind),
		SubjectToEPF:             req.SubjectToEPF,
		SubjectToSOCSO:           req.SubjectToSOCSO,
		SubjectToEIS:             req.SubjectToEIS,
		SubjectToPCB:             req.SubjectToPCB,
		SubjectToBPJSKes:         req.SubjectToBPJSKes,
		SubjectToJHT:             req.SubjectToJHT,
		SubjectToJKK:             req.SubjectToJKK,
		SubjectToJKM:             req.SubjectToJKM,
		SubjectToJP:              req.SubjectToJP,
		SubjectToPPh21:           req.SubjectToPPh21,
		ReducesContributionWages: req.ReducesContributionWages,
	})
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListPayrollPresets(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopePayrollPreview, " to read payroll presets") {
		return
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	presets, next, err := ps.ListPayrollPresets(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	if presets == nil {
		presets = []store.PayrollItemPreset{}
	}
	writePage(w, presets, next)
}

type patchPayrollPresetBody struct {
	Name                     *string `json:"name,omitempty"`
	Kind                     *string `json:"kind,omitempty"`
	SubjectToEPF             *bool   `json:"subject_to_epf,omitempty"`
	SubjectToSOCSO           *bool   `json:"subject_to_socso,omitempty"`
	SubjectToEIS             *bool   `json:"subject_to_eis,omitempty"`
	SubjectToPCB             *bool   `json:"subject_to_pcb,omitempty"`
	SubjectToBPJSKes         *bool   `json:"subject_to_bpjs_kes,omitempty"`
	SubjectToJHT             *bool   `json:"subject_to_jht,omitempty"`
	SubjectToJKK             *bool   `json:"subject_to_jkk,omitempty"`
	SubjectToJKM             *bool   `json:"subject_to_jkm,omitempty"`
	SubjectToJP              *bool   `json:"subject_to_jp,omitempty"`
	SubjectToPPh21           *bool   `json:"subject_to_pph21,omitempty"`
	ReducesContributionWages *bool   `json:"reduces_contribution_wages,omitempty"`
	Version                  int64   `json:"version"`
}

func (s *Server) patchPayrollPreset(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requirePayrollRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopePayrollPreview, " to manage payroll presets") {
		return zero, false
	}
	ps, ok := s.payrollStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req patchPayrollPresetBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := ps.GetPayrollPreset(r.Context(), orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return zero, false
	}
	if cur.IsSystem {
		writeError(w, r, http.StatusConflict, "SYSTEM_PRESET_IMMUTABLE",
			"system presets are immutable; create an organization preset instead")
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.Name != nil {
		cur.Name = *req.Name
	}
	if req.Kind != nil {
		cur.Kind = payroll.PresetKind(*req.Kind)
	}
	if req.SubjectToEPF != nil {
		cur.SubjectToEPF = *req.SubjectToEPF
	}
	if req.SubjectToSOCSO != nil {
		cur.SubjectToSOCSO = *req.SubjectToSOCSO
	}
	if req.SubjectToEIS != nil {
		cur.SubjectToEIS = *req.SubjectToEIS
	}
	if req.SubjectToPCB != nil {
		cur.SubjectToPCB = *req.SubjectToPCB
	}
	if req.SubjectToBPJSKes != nil {
		cur.SubjectToBPJSKes = *req.SubjectToBPJSKes
	}
	if req.SubjectToJHT != nil {
		cur.SubjectToJHT = *req.SubjectToJHT
	}
	if req.SubjectToJKK != nil {
		cur.SubjectToJKK = *req.SubjectToJKK
	}
	if req.SubjectToJKM != nil {
		cur.SubjectToJKM = *req.SubjectToJKM
	}
	if req.SubjectToJP != nil {
		cur.SubjectToJP = *req.SubjectToJP
	}
	if req.SubjectToPPh21 != nil {
		cur.SubjectToPPh21 = *req.SubjectToPPh21
	}
	if req.ReducesContributionWages != nil {
		cur.ReducesContributionWages = *req.ReducesContributionWages
	}
	updated, err := ps.UpdatePayrollPreset(r.Context(), orgID, cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}
