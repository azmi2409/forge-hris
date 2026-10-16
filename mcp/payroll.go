package mcp

// MCP payroll tools (component 5, wave 3). REST and MCP share the same store
// calls inside the handlers, per the repo's established pattern: the REST
// sibling mirrors these flows (notably the submit-gate algorithm), so both
// surfaces call store.PayrollStore + the pure payroll package directly.
//
// Two-phase gate: hris.preview_payroll dry-runs payroll.Calculate, diffs
// against the previous run's lines, persists the computed pay lines on the
// draft run, and saves a 30-minute preview payload. hris.submit_payroll
// (TierNever) requires that preview_id plus the run's current version and
// moves draft -> approved, fanning one payslip.issued webhook per pay line.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/payroll"
	"github.com/azmi2409/forge-hris/store"
)

// previewTTL bounds how long a payroll preview stays submittable.
const previewTTL = 30 * time.Minute

// payrollEmployeeTotal is one row of the per-employee totals snapshot stored
// in a payroll preview payload and returned by the preview tools.
type payrollEmployeeTotal struct {
	EmployeeID      string `json:"employee_id"`
	GrossMinor      int64  `json:"gross_minor"`
	AdditionsMinor  int64  `json:"additions_minor"`
	DeductionsMinor int64  `json:"deductions_minor"`
	NetMinor        int64  `json:"net_minor"`
}

// payrollPreviewPayload is the JSON stored via SavePayrollPreview: the
// calculation snapshot the submit gate replays. The stored preview is what
// makes the two-phase gate checkable (run match, expiry) without trusting
// caller-supplied totals.
type payrollPreviewPayload struct {
	RunID           string                    `json:"run_id"`
	Period          string                    `json:"period"`
	CountryCode     string                    `json:"country_code"`
	RulePackVersion string                    `json:"rule_pack_version"`
	CalculatedAt    time.Time                 `json:"calculated_at"`
	Totals          []payrollEmployeeTotal    `json:"totals"`
	Previews        []payroll.EmployeePreview `json:"previews"`
	Anomalies       []payroll.Anomaly         `json:"anomalies"`
	AnomalyCount    int                       `json:"anomaly_count"`
}

// --- argument helpers ---

// reqInt64Arg reads a required non-negative integer minor-unit amount from
// JSON-decoded args (float64 from the wire).
func reqInt64Arg(args map[string]any, key string) (int64, *ToolError) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, validationErr(key, "required")
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != float64(int64(f)) {
		return 0, validationErr(key, "must be a non-negative integer")
	}
	return int64(f), nil
}

// optInt64Arg reads an optional non-negative integer amount; absent is 0.
func optInt64Arg(args map[string]any, key string) (int64, *ToolError) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, nil
	}
	return reqInt64Arg(args, key)
}

// optIntArg reads an optional non-negative integer; absent is 0.
func optIntArg(args map[string]any, key string) (int, *ToolError) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != float64(int64(f)) {
		return 0, validationErr(key, "must be a non-negative integer")
	}
	return int(f), nil
}

// parsePeriodArg validates a YYYY-MM period argument.
func parsePeriodArg(args map[string]any, key string, required bool) (string, *ToolError) {
	s := optString(args, key)
	if s == "" {
		if required {
			return "", validationErr(key, "required (YYYY-MM)")
		}
		return "", nil
	}
	if _, err := time.Parse("2006-01", s); err != nil {
		return "", validationErr(key, "must be YYYY-MM")
	}
	return s, nil
}

// actorLabel is the identity string recorded on store transitions: the
// employee behind the credential when there is one, else the credential ID.
func actorLabel(ai *AuthInfo) string {
	if ai.EmployeeID != nil && *ai.EmployeeID != "" {
		return *ai.EmployeeID
	}
	return ai.CredentialID
}

// payrollCatalog loads the org's item catalog (seed presets + org rows) and
// converts it to the shared payroll.Preset contract the engine needs.
func (s *Server) payrollCatalog(ctx context.Context, orgID string) ([]payroll.Preset, *ToolError) {
	var all []store.PayrollItemPreset
	cursor := ""
	for {
		batch, next, err := s.store.ListPayrollPresets(ctx, orgID, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, mapStoreError(err)
		}
		all = append(all, batch...)
		if next == "" {
			break
		}
		cursor = next
	}
	presets := make([]payroll.Preset, 0, len(all))
	for _, p := range all {
		presets = append(presets, store.PresetToPayroll(p))
	}
	return presets, nil
}

// listAllPayLines pages through a run's pay lines.
func (s *Server) listAllPayLines(ctx context.Context, orgID, runID string) ([]store.EmployeePayLine, *ToolError) {
	var all []store.EmployeePayLine
	cursor := ""
	for {
		batch, next, err := s.store.ListPayLines(ctx, orgID, runID, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, mapStoreError(err)
		}
		all = append(all, batch...)
		if next == "" {
			break
		}
		cursor = next
	}
	return all, nil
}

// previousRunResults loads the latest non-draft run before the given run
// (by period) and maps its pay lines to PayResults for DiffRunPreview.
// A nil result means "no prior run" — DiffRunPreview flags accordingly.
func (s *Server) previousRunResults(ctx context.Context, orgID string, run store.PayrollRun) ([]payroll.PayResult, *ToolError) {
	var best *store.PayrollRun
	cursor := ""
	for {
		batch, next, err := s.store.ListPayrollRuns(ctx, orgID, store.PayrollRunFilter{}, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, mapStoreError(err)
		}
		for i := range batch {
			r := batch[i]
			if r.Period >= run.Period || r.Status == store.PayrollRunDraft {
				continue
			}
			if best == nil || r.Period > best.Period {
				c := r
				best = &c
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if best == nil {
		return nil, nil
	}
	lines, terr := s.listAllPayLines(ctx, orgID, best.ID)
	if terr != nil {
		return nil, terr
	}
	out := make([]payroll.PayResult, 0, len(lines))
	for _, l := range lines {
		out = append(out, payroll.PayResult{
			EmployeeID: l.EmployeeID,
			NetMinor:   l.NetMinor,
			Warnings:   bankWarnings(l),
		})
	}
	return out, nil
}

// bankWarnings reconstructs the DiffRunPreview-relevant warnings for a
// stored line: a line without bank details gets MISSING_BANK_DETAILS, the
// same warning Calculate would have emitted.
func bankWarnings(l store.EmployeePayLine) []string {
	if strings.TrimSpace(l.BankAccount) == "" || strings.TrimSpace(l.BankCode) == "" {
		return []string{payroll.WarnMissingBankDetails}
	}
	return nil
}

// payrollPayInput parses one employee element of the preview input into a
// payroll.PayInput. Money arrives as JSON numbers (float64) and must be
// integral; unpaid_leave_days may be fractional.
func payrollPayInput(emp map[string]any) (payroll.PayInput, *ToolError) {
	var in payroll.PayInput
	id, ok := emp["employee_id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return in, validationErr("employees[].employee_id", "required")
	}
	in.EmployeeID = strings.TrimSpace(id)
	m := map[string]any(emp)
	var terr *ToolError
	if in.BaseSalaryMinor, terr = optInt64Arg(m, "base_salary_minor"); terr != nil {
		return in, terr
	}
	if in.DailyRateMinor, terr = optInt64Arg(m, "daily_rate_minor"); terr != nil {
		return in, terr
	}
	if in.UnpaidLeaveDays, terr = optFloat(m, "unpaid_leave_days"); terr != nil {
		return in, terr
	}
	if in.UnpaidLeaveDays < 0 {
		return in, validationErr("employees[].unpaid_leave_days", "must be non-negative")
	}
	if in.AgeYears, terr = optIntArg(m, "age_years"); terr != nil {
		return in, terr
	}
	if in.JKKRiskClass, terr = optIntArg(m, "jkk_risk_class"); terr != nil {
		return in, terr
	}
	in.CitizenMY = optBool(m, "citizen_my")
	in.PRMY = optBool(m, "pr_my")
	in.ForeignWorkerMY = optBool(m, "foreign_worker_my")
	in.SKBBKOptIn = optBool(m, "skbbk_opt_in")
	in.EmployerBornePCB = optBool(m, "employer_borne_pcb")
	in.PTKPStatus = optString(m, "ptkp_status")
	in.BankAccount = strings.TrimSpace(optString(m, "bank_account"))
	in.BankCode = strings.TrimSpace(optString(m, "bank_code"))

	rawItems, ok := m["items"]
	if !ok || rawItems == nil {
		return in, nil
	}
	items, ok := rawItems.([]any)
	if !ok {
		return in, validationErr("employees[].items", "must be an array")
	}
	for i, raw := range items {
		im, ok := raw.(map[string]any)
		if !ok {
			return in, validationErr("employees[].items", "must be objects")
		}
		code, ok := im["preset_code"].(string)
		if !ok || strings.TrimSpace(code) == "" {
			return in, validationErr("employees[].items["+itoa(i)+"].preset_code", "required")
		}
		amount, terr := reqInt64Arg(im, "amount_minor")
		if terr != nil {
			return in, validationErr("employees[].items["+itoa(i)+"].amount_minor", "required, non-negative integer")
		}
		in.Items = append(in.Items, payroll.PayItemInput{
			PresetCode:  strings.TrimSpace(code),
			AmountMinor: amount,
		})
	}
	return in, nil
}

// payrollEmployeeSchema is the shared per-employee calculate input shape for
// hris.preview_payroll. unpaid_leave_days is accepted explicitly here; the
// REST surface auto-derives it from approved unpaid leave instead.
func payrollEmployeeSchema() map[string]any {
	itemObj := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"preset_code":  strProp("Item preset code, e.g. basic_salary, overtime_15, transport_allowance."),
			"amount_minor": intProp("Amount in minor units (sen for MYR, rupiah for IDR)."),
		},
		"required": []any{"preset_code", "amount_minor"},
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"employee_id":        strProp("Employee ID."),
			"base_salary_minor":  intProp("Base monthly salary in minor units (sen for MYR, rupiah for IDR)."),
			"items":              arrProp(itemObj, "Payroll items (additions/deductions by preset code)."),
			"unpaid_leave_days":  numProp("Unpaid leave days to deduct (REST auto-derives this from approved unpaid leave; MCP callers supply it explicitly)."),
			"daily_rate_minor":   intProp("Daily rate used for the unpaid-leave deduction; omitted (0) warns and deducts nothing."),
			"age_years":          intProp("Employee age in whole years (drives EPF/SOCSO brackets)."),
			"citizen_my":         boolProp("Malaysian citizen (KWSP)."),
			"pr_my":              boolProp("Malaysian permanent resident (KWSP)."),
			"foreign_worker_my":  boolProp("Non-Malaysian employee (2%/2% EPF regime)."),
			"skbbk_opt_in":       boolProp("Opted in to Lindung 24 Jam (locals only; foreign workers are always covered)."),
			"employer_borne_pcb": boolProp("Employer bears PCB (adds it back to net)."),
			"jkk_risk_class":     intProp("JKK risk class 1..5 (Indonesia)."),
			"ptkp_status":        strProp("PTKP status, e.g. TK/0 (Indonesia; unknown defaults to TER category A with a warning)."),
			"bank_account":       strProp("Bank account number for the bank file."),
			"bank_code":          strProp("Bank code for the bank file."),
		},
		"required": []any{"employee_id"},
	}
}

// --- payroll tool handlers ---

func (s *Server) handleListPayrollRuns(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	f := store.PayrollRunFilter{}
	if st := optString(args, "status"); st != "" {
		f.Status = store.PayrollRunStatus(st)
		if !store.ValidPayrollRunStatus(f.Status) {
			return nil, nil, validationErr("status", "must be one of draft, approved, locked, cancelled")
		}
	}
	var terr *ToolError
	if f.Period, terr = parsePeriodArg(args, "period", false); terr != nil {
		return nil, nil, terr
	}
	p, terr := parsePageArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	runs, next, err := s.store.ListPayrollRuns(ctx, ai.OrgID, f, p)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if runs == nil {
		runs = []store.PayrollRun{}
	}
	return pageEnvelope(runs, next),
		&auditRecord{action: "payroll_run.listed", resourceType: "payroll_run"}, nil
}

func (s *Server) handleGetPayrollRun(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	runID, terr := reqString(args, "run_id")
	if terr != nil {
		return nil, nil, terr
	}
	run, err := s.store.GetPayrollRun(ctx, ai.OrgID, runID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	out := map[string]any{"payroll_run": run}
	if optBool(args, "lines") {
		lines, terr := s.listAllPayLines(ctx, ai.OrgID, runID)
		if terr != nil {
			return nil, nil, terr
		}
		out["pay_lines"] = lines
	}
	return out,
		&auditRecord{action: "payroll_run.read", resourceType: "payroll_run", resourceID: run.ID}, nil
}

func (s *Server) handleDraftPayrollRun(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	period, terr := parsePeriodArg(args, "period", true)
	if terr != nil {
		return nil, nil, terr
	}
	country, terr := reqString(args, "country_code")
	if terr != nil {
		return nil, nil, terr
	}
	if country != "MY" && country != "ID" {
		return nil, nil, validationErr("country_code", "must be MY or ID")
	}
	run, err := s.store.CreatePayrollRun(ctx, store.PayrollRun{
		OrganizationID:  ai.OrgID,
		Period:          period,
		CountryCode:     country,
		RulePackVersion: payroll.RulePackVersion(country),
	})
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if _, err := s.store.EnqueueWebhookEvent(ctx, ai.OrgID, store.WebhookEventPayrollDrafted, map[string]any{
		"run_id": run.ID, "period": run.Period, "country_code": run.CountryCode, "via": "mcp",
	}); err != nil {
		s.logger.Error("mcp webhook enqueue failed", "tool", "hris.draft_payroll_run", "error", err)
	}
	return map[string]any{"payroll_run": run},
		&auditRecord{action: "payroll_run.drafted", resourceType: "payroll_run", resourceID: run.ID, after: run}, nil
}

// handlePreviewPayroll dry-runs the calculation for a draft run: it builds
// the engine inputs from the arguments, diffs the results against the
// previous run's lines, persists the computed pay lines on the draft run,
// and saves a 30-minute preview payload that hris.submit_payroll requires.
func (s *Server) handlePreviewPayroll(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	runID, terr := reqString(args, "run_id")
	if terr != nil {
		return nil, nil, terr
	}
	run, err := s.store.GetPayrollRun(ctx, ai.OrgID, runID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if run.Status != store.PayrollRunDraft {
		return nil, nil, toolErr("RUN_NOT_DRAFT", "payroll run "+run.ID+" is "+string(run.Status)+", not draft")
	}
	rawEmps, ok := args["employees"].([]any)
	if !ok || len(rawEmps) == 0 {
		return nil, nil, validationErr("employees", "at least one employee is required")
	}
	inputs := make([]payroll.PayInput, 0, len(rawEmps))
	for i, raw := range rawEmps {
		em, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, validationErr("employees["+itoa(i)+"]", "must be an object")
		}
		in, terr := payrollPayInput(em)
		if terr != nil {
			return nil, nil, terr
		}
		inputs = append(inputs, in)
	}
	presets, terr := s.payrollCatalog(ctx, ai.OrgID)
	if terr != nil {
		return nil, nil, terr
	}
	results, err := payroll.Calculate(run.CountryCode, presets, inputs)
	if err != nil {
		return nil, nil, toolErr("CALCULATION_ERROR", err.Error())
	}
	prevResults, terr := s.previousRunResults(ctx, ai.OrgID, run)
	if terr != nil {
		return nil, nil, terr
	}
	previews := payroll.DiffRunPreview(prevResults, results)
	if previews == nil {
		previews = []payroll.EmployeePreview{}
	}
	var anomalies []payroll.Anomaly
	for _, ep := range previews {
		anomalies = append(anomalies, ep.Anomalies...)
	}
	if anomalies == nil {
		anomalies = []payroll.Anomaly{}
	}

	// Persist the computed lines on the draft run so the submit gate has
	// something to approve and the payslip.issued fan-out has per-line data.
	bankByEmp := map[string][2]string{}
	for _, in := range inputs {
		bankByEmp[in.EmployeeID] = [2]string{in.BankAccount, in.BankCode}
	}
	totals := make([]payrollEmployeeTotal, 0, len(results))
	for _, r := range results {
		bank := bankByEmp[r.EmployeeID]
		line := store.EmployeePayLine{
			OrganizationID:  ai.OrgID,
			RunID:           run.ID,
			EmployeeID:      r.EmployeeID,
			GrossMinor:      r.GrossMinor,
			AdditionsMinor:  r.AdditionsMinor,
			DeductionsMinor: r.DeductionsMinor,
			NetMinor:        r.NetMinor,
			Items:           r.Items,
			Statutory:       r.Splits,
			BankAccount:     bank[0],
			BankCode:        bank[1],
		}
		// Re-previewing the same run updates the existing line: carry the
		// stored version for the optimistic-concurrency check.
		if cur, err := s.store.GetPayLine(ctx, ai.OrgID, run.ID, r.EmployeeID); err == nil {
			line.ID = cur.ID
			line.Version = cur.Version
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, nil, mapStoreError(err)
		}
		stored, err := s.store.UpsertPayLine(ctx, line)
		if err != nil {
			return nil, nil, mapStoreError(err)
		}
		_ = stored
		totals = append(totals, payrollEmployeeTotal{
			EmployeeID:      r.EmployeeID,
			GrossMinor:      r.GrossMinor,
			AdditionsMinor:  r.AdditionsMinor,
			DeductionsMinor: r.DeductionsMinor,
			NetMinor:        r.NetMinor,
		})
	}

	payload := payrollPreviewPayload{
		RunID:           run.ID,
		Period:          run.Period,
		CountryCode:     run.CountryCode,
		RulePackVersion: payroll.RulePackVersion(run.CountryCode),
		CalculatedAt:    time.Now().UTC(),
		Totals:          totals,
		Previews:        previews,
		Anomalies:       anomalies,
		AnomalyCount:    len(anomalies),
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, toolErr("INTERNAL", "internal error")
	}
	pv, err := s.store.SavePayrollPreview(ctx, store.PayrollPreview{
		OrganizationID: ai.OrgID,
		RunID:          run.ID,
		Payload:        rawPayload,
		ExpiresAt:      time.Now().UTC().Add(previewTTL),
	})
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if _, err := s.store.EnqueueWebhookEvent(ctx, ai.OrgID, store.WebhookEventPayrollCalculated, map[string]any{
		"run_id": run.ID, "period": run.Period, "employee_count": len(totals),
		"anomaly_count": len(anomalies), "via": "mcp",
	}); err != nil {
		s.logger.Error("mcp webhook enqueue failed", "tool", "hris.preview_payroll", "error", err)
	}
	return map[string]any{
		"preview_id":        pv.ID,
		"run_id":            run.ID,
		"period":            run.Period,
		"country_code":      run.CountryCode,
		"rule_pack_version": payload.RulePackVersion,
		"totals":            totals,
		"previews":          previews,
		"anomalies":         anomalies,
		"anomaly_count":     len(anomalies),
		"expires_at":        pv.ExpiresAt.Format(time.RFC3339),
		"note":              "preview saved; submit with hris.submit_payroll (preview_id + the run's current version) within 30 minutes",
	}, &auditRecord{action: "payroll_run.calculated", resourceType: "payroll_run", resourceID: run.ID, after: payload}, nil
}

// checkSubmitGate loads the run and preview and applies the read-only half
// of the submit gate, shared by the preview function and the handler.
func (s *Server) checkSubmitGate(ctx context.Context, ai *AuthInfo, runID, previewID string) (store.PayrollRun, store.PayrollPreview, payrollPreviewPayload, *ToolError) {
	var payload payrollPreviewPayload
	run, err := s.store.GetPayrollRun(ctx, ai.OrgID, runID)
	if err != nil {
		return run, store.PayrollPreview{}, payload, mapStoreError(err)
	}
	if run.Status != store.PayrollRunDraft {
		return run, store.PayrollPreview{}, payload,
			toolErr("RUN_NOT_DRAFT", "payroll run "+run.ID+" is "+string(run.Status)+", not draft")
	}
	prev, err := s.store.GetPayrollPreview(ctx, ai.OrgID, previewID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return run, prev, payload, toolErr("PREVIEW_EXPIRED", "preview expired or unknown; run hris.preview_payroll again")
		}
		return run, prev, payload, mapStoreError(err)
	}
	if prev.RunID != runID {
		return run, prev, payload, toolErr("PREVIEW_MISMATCH", "preview belongs to a different run")
	}
	if err := json.Unmarshal(prev.Payload, &payload); err != nil {
		return run, prev, payload, toolErr("INTERNAL", "internal error")
	}
	return run, prev, payload, nil
}

// previewSubmitPayroll builds the human-readable preview for the TierNever
// submit gate: totals plus the anomaly count, without mutating anything.
func (s *Server) previewSubmitPayroll(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *ToolError) {
	runID, terr := reqString(args, "run_id")
	if terr != nil {
		return nil, terr
	}
	previewID, terr := reqString(args, "preview_id")
	if terr != nil {
		return nil, terr
	}
	run, prev, payload, terr := s.checkSubmitGate(ctx, ai, runID, previewID)
	if terr != nil {
		return nil, terr
	}
	return map[string]any{
		"summary": "Submit payroll run " + run.Period + " (" + run.CountryCode + ", rule pack " +
			payload.RulePackVersion + "): draft -> approved; " + itoa(len(payload.Totals)) +
			" payslips will be issued, " + itoa(payload.AnomalyCount) + " anomalies flagged",
		"run_id":             run.ID,
		"period":             run.Period,
		"country_code":       run.CountryCode,
		"version":            run.Version,
		"preview_id":         prev.ID,
		"preview_expires_at": prev.ExpiresAt.Format(time.RFC3339),
		"totals":             payload.Totals,
		"previews":           payload.Previews,
		"anomaly_count":      payload.AnomalyCount,
		"anomalies":          payload.Anomalies,
		"webhooks":           "one payroll.submitted plus one payslip.issued per pay line",
	}, nil
}

// handleSubmitPayroll executes the two-phase submit gate exactly as the REST
// sibling mirrors it: the run must be draft, the preview must be live and
// belong to this run, then draft -> approved fans the webhooks.
func (s *Server) handleSubmitPayroll(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	runID, terr := reqString(args, "run_id")
	if terr != nil {
		return nil, nil, terr
	}
	previewID, terr := reqString(args, "preview_id")
	if terr != nil {
		return nil, nil, terr
	}
	version, terr := reqVersion(args)
	if terr != nil {
		return nil, nil, terr
	}
	run, _, payload, terr := s.checkSubmitGate(ctx, ai, runID, previewID)
	if terr != nil {
		return nil, nil, terr
	}
	run, err := s.store.TransitionPayrollRun(ctx, ai.OrgID, runID, version, store.PayrollRunApproved, actorLabel(ai))
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if _, err := s.store.EnqueueWebhookEvent(ctx, ai.OrgID, store.WebhookEventPayrollSubmitted, map[string]any{
		"run_id": run.ID, "period": run.Period, "via": "mcp",
	}); err != nil {
		s.logger.Error("mcp webhook enqueue failed", "tool", "hris.submit_payroll", "error", err)
	}
	lines, terr := s.listAllPayLines(ctx, ai.OrgID, runID)
	if terr != nil {
		return nil, nil, terr
	}
	issued := 0
	for _, l := range lines {
		if _, err := s.store.EnqueueWebhookEvent(ctx, ai.OrgID, store.WebhookEventPayslipIssued, map[string]any{
			"run_id": run.ID, "employee_id": l.EmployeeID, "via": "mcp",
		}); err != nil {
			s.logger.Error("mcp webhook enqueue failed",
				"tool", "hris.submit_payroll", "event", "payslip.issued", "error", err)
		} else {
			issued++
		}
	}
	return map[string]any{
		"payroll_run":     run,
		"payslips_issued": issued,
		"anomaly_count":   payload.AnomalyCount,
	}, &auditRecord{action: "payroll.submitted", resourceType: "payroll_run", resourceID: run.ID, after: run}, nil
}

func (s *Server) previewCancelPayrollRun(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *ToolError) {
	runID, terr := reqString(args, "run_id")
	if terr != nil {
		return nil, terr
	}
	run, err := s.store.GetPayrollRun(ctx, ai.OrgID, runID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return map[string]any{
		"summary": "Cancel payroll run " + run.Period + " (" + run.CountryCode + "): " +
			string(run.Status) + " -> cancelled; computed pay lines are kept for audit",
		"run_id": run.ID,
		"status": string(run.Status),
	}, nil
}

func (s *Server) handleCancelPayrollRun(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	runID, terr := reqString(args, "run_id")
	if terr != nil {
		return nil, nil, terr
	}
	version, terr := reqVersion(args)
	if terr != nil {
		return nil, nil, terr
	}
	before, err := s.store.GetPayrollRun(ctx, ai.OrgID, runID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	run, err := s.store.TransitionPayrollRun(ctx, ai.OrgID, runID, version, store.PayrollRunCancelled, actorLabel(ai))
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if _, err := s.store.EnqueueWebhookEvent(ctx, ai.OrgID, store.WebhookEventPayrollCancelled, map[string]any{
		"run_id": run.ID, "period": run.Period, "from_status": string(before.Status), "via": "mcp",
	}); err != nil {
		s.logger.Error("mcp webhook enqueue failed", "tool", "hris.cancel_payroll_run", "error", err)
	}
	return map[string]any{"payroll_run": run},
		&auditRecord{action: "payroll_run.cancelled", resourceType: "payroll_run", resourceID: run.ID, before: before, after: run}, nil
}

// handleGetPayslip returns one employee's pay line for a run. Members may
// only fetch their own payslip (mirrors handleGetEmployee's self-check).
func (s *Server) handleGetPayslip(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	runID, terr := reqString(args, "run_id")
	if terr != nil {
		return nil, nil, terr
	}
	employeeID, terr := reqString(args, "employee_id")
	if terr != nil {
		return nil, nil, terr
	}
	if effectiveRole(ai) == store.RoleMember &&
		(ai.EmployeeID == nil || *ai.EmployeeID != employeeID) {
		return nil, nil, toolErr("FORBIDDEN", "members may only read their own payslip")
	}
	line, err := s.store.GetPayLine(ctx, ai.OrgID, runID, employeeID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"payslip": line},
		&auditRecord{action: "payslip.read", resourceType: "payslip", resourceID: line.ID}, nil
}

// handleListPaySchedules lists the org's pay-period definitions (the
// attendance cadences) and states the payroll cadence: payroll runs are
// calendar-month (YYYY-MM), one run per month, regardless of the
// attendance cadence.
func (s *Server) handleListPaySchedules(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	var defs []store.PayPeriodDefinition
	cursor := ""
	for {
		batch, next, err := s.store.ListPayPeriodDefinitions(ctx, ai.OrgID, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, nil, mapStoreError(err)
		}
		defs = append(defs, batch...)
		if next == "" {
			break
		}
		cursor = next
	}
	if defs == nil {
		defs = []store.PayPeriodDefinition{}
	}
	return map[string]any{
		"pay_schedules": defs,
		"payroll_cadence": map[string]any{
			"cadence": "monthly",
			"period":  "calendar month, YYYY-MM",
			"note":    "Payroll runs are one calendar month each. The pay-period definitions above drive attendance timesheets; payroll does not inherit their cadence.",
		},
	}, &auditRecord{action: "pay_schedule.listed", resourceType: "pay_schedule"}, nil
}

// payrollTools returns the 8 component-5 tool definitions. Registration in
// newRegistry appends these to the base 29.
func payrollTools(s *Server) []*ToolDef {
	return []*ToolDef{
		{
			Name:        "hris.list_payroll_runs",
			Description: "List payroll runs with optional status/period filters. Cursor-paginated.",
			InputSchema: objSchema(map[string]any{
				"status": strProp("Filter by status: draft, approved, locked, cancelled."),
				"period": strProp("Filter by period (YYYY-MM)."),
				"limit":  pageProps()["limit"],
				"cursor": pageProps()["cursor"],
			}),
			OutputSchema:  pageResult("Matching payroll runs."),
			RequiredScope: store.ScopePayrollRead,
			Tier:          TierAuto,
			Annotations:   readOnly("List payroll runs"),
			Handler:       s.handleListPayrollRuns,
		},
		{
			Name:        "hris.get_payroll_run",
			Description: "Get one payroll run by ID; set lines:true to attach its computed pay lines.",
			InputSchema: objSchema(map[string]any{
				"run_id": strProp("Payroll run ID."),
				"lines":  boolProp("Attach the run's computed pay lines."),
			}, "run_id"),
			OutputSchema: objSchema(map[string]any{
				"payroll_run": map[string]any{"type": "object"},
				"pay_lines":   arrProp(map[string]any{"type": "object"}, "Computed pay lines (when lines:true)."),
			}),
			RequiredScope: store.ScopePayrollRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get payroll run"),
			Handler:       s.handleGetPayrollRun,
		},
		{
			Name:        "hris.draft_payroll_run",
			Description: "Draft a payroll run for a YYYY-MM period and country (MY|ID). One run per period.",
			InputSchema: objSchema(map[string]any{
				"period":       strProp("Payroll period (YYYY-MM)."),
				"country_code": strProp("MY or ID."),
			}, "period", "country_code"),
			OutputSchema: objSchema(map[string]any{
				"payroll_run": map[string]any{"type": "object"},
			}),
			RequiredScope: store.ScopePayrollPreview,
			Tier:          TierAuto,
			Annotations:   write("Draft payroll run"),
			Handler:       s.handleDraftPayrollRun,
		},
		{
			Name: "hris.preview_payroll",
			Description: "Dry-run the payroll calculation for a draft run: computes gross/net per employee, diffs " +
				"against the previous run with anomaly flags, persists the pay lines on the draft, and saves a " +
				"30-minute preview payload. Must precede hris.submit_payroll. NOTE: unpaid_leave_days is supplied " +
				"explicitly here; the REST surface auto-derives it from approved unpaid leave.",
			InputSchema: objSchema(map[string]any{
				"run_id":    strProp("Draft payroll run ID."),
				"employees": arrProp(payrollEmployeeSchema(), "Per-employee calculate input."),
			}, "run_id", "employees"),
			OutputSchema: objSchema(map[string]any{
				"preview_id":        strProp("Preview ID for hris.submit_payroll (30-minute TTL)."),
				"run_id":            strProp("Payroll run ID."),
				"period":            strProp("Payroll period (YYYY-MM)."),
				"country_code":      strProp("MY or ID."),
				"rule_pack_version": strProp("Statutory rule pack version used."),
				"totals":            arrProp(map[string]any{"type": "object"}, "Per-employee gross/additions/deductions/net."),
				"anomalies":         arrProp(map[string]any{"type": "object"}, "Anomaly flags vs the previous run."),
				"anomaly_count":     intProp("Number of anomalies."),
				"expires_at":        strProp("Preview expiry (RFC3339)."),
			}),
			RequiredScope: store.ScopePayrollPreview,
			Tier:          TierAuto,
			Annotations:   write("Preview payroll"),
			Handler:       s.handlePreviewPayroll,
		},
		{
			Name: "hris.submit_payroll",
			Description: "Submit a payroll run (draft -> approved). NEVER auto-approved: the first call returns a " +
				"preview with totals and the anomaly count; the follow-up must carry the resume token AND " +
				"confirmed:true. Requires a live preview from hris.preview_payroll (preview_id + the run's " +
				"current version). Fans one payslip.issued webhook per pay line.",
			InputSchema: objSchema(map[string]any{
				"run_id":     strProp("Draft payroll run ID."),
				"preview_id": strProp("Preview ID from hris.preview_payroll (30-minute TTL)."),
				"version":    versionProp(),
				"confirmed":  boolProp("Set true on the follow-up call, together with the resume token, to execute."),
			}, "run_id", "preview_id", "version"),
			OutputSchema: objSchema(map[string]any{
				"payroll_run":     map[string]any{"type": "object"},
				"payslips_issued": intProp("Number of payslip.issued webhooks fanned out."),
				"anomaly_count":   intProp("Anomalies on the approved preview."),
			}),
			RequiredScope: store.ScopePayrollSubmit,
			MinRole:       store.RoleAdmin,
			Tier:          TierNever,
			Annotations:   ToolAnnotations{DestructiveHint: true, Title: "Submit payroll"},
			Handler:       s.handleSubmitPayroll,
			Preview:       s.previewSubmitPayroll,
		},
		{
			Name:        "hris.cancel_payroll_run",
			Description: "Cancel a payroll run (draft/approved -> cancelled). Computed pay lines are kept for audit. Requires human approval.",
			InputSchema: objSchema(map[string]any{
				"run_id":  strProp("Payroll run ID."),
				"version": versionProp(),
			}, "run_id", "version"),
			OutputSchema: objSchema(map[string]any{
				"payroll_run": map[string]any{"type": "object"},
			}),
			RequiredScope: store.ScopePayrollSubmit,
			MinRole:       store.RoleAdmin,
			Tier:          TierConfirm,
			Annotations:   ToolAnnotations{DestructiveHint: true, Title: "Cancel payroll run"},
			Handler:       s.handleCancelPayrollRun,
			Preview:       s.previewCancelPayrollRun,
		},
		{
			Name:        "hris.get_payslip",
			Description: "Get one employee's payslip (computed pay line) for a run. Members may only read their own.",
			InputSchema: objSchema(map[string]any{
				"run_id":      strProp("Payroll run ID."),
				"employee_id": strProp("Employee ID."),
			}, "run_id", "employee_id"),
			OutputSchema: objSchema(map[string]any{
				"payslip": map[string]any{"type": "object", "description": "The employee's computed pay line."},
			}),
			RequiredScope: store.ScopePayrollRead,
			Tier:          TierAuto,
			Annotations:   readOnly("Get payslip"),
			Handler:       s.handleGetPayslip,
		},
		{
			Name:        "hris.list_pay_schedules",
			Description: "List the org's pay-period definitions (attendance cadences) plus the monthly payroll cadence note: payroll runs are calendar-month (YYYY-MM), one run per month.",
			InputSchema: objSchema(map[string]any{}),
			OutputSchema: objSchema(map[string]any{
				"pay_schedules":   arrProp(map[string]any{"type": "object"}, "Pay-period definitions."),
				"payroll_cadence": map[string]any{"type": "object", "description": "Monthly payroll cadence descriptor."},
			}),
			RequiredScope: store.ScopePayrollRead,
			Tier:          TierAuto,
			Annotations:   readOnly("List pay schedules"),
			Handler:       s.handleListPaySchedules,
		},
	}
}
