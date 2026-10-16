// Component 5 (payroll) REST API tests: run lifecycle
// draft -> calculate -> preview -> submit -> lock, the two-phase submit
// gate, RBAC, cross-tenant isolation, unpaid-leave auto-deduction, sync
// items, presets, payslips, bank files, and statutory exports.
package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// payrollFixture holds one org plus a token and employee per role, a
// second org for cross-tenant checks, and the store for direct setup.
type payrollFixture struct {
	srv        *Server
	st         store.Store
	orgID      string
	tokens     map[string]string
	empIDs     map[string]string
	crossToken string
}

func newPayrollFixture(t *testing.T, slug string) *payrollFixture {
	t.Helper()
	srv, st := newTestServer(t)
	a := bootstrap(t, srv, slug)
	b := bootstrap(t, srv, slug+"-x")
	f := &payrollFixture{
		srv: srv, st: st,
		orgID:      a.Organization.ID,
		tokens:     map[string]string{"owner": a.Token},
		empIDs:     map[string]string{},
		crossToken: b.Token,
	}
	for _, r := range []struct {
		label string
		role  store.Role
	}{
		{"admin", store.RoleAdmin},
		{"manager", store.RoleManager},
		{"member", store.RoleMember},
	} {
		empID := joinedEmployee(t, st, a.Organization.ID, "P-"+r.label, uniqEmail("pay-"+r.label), r.role)
		plain := "payroll-tok-" + r.label + "-" + slug
		mintToken(t, st, a.Organization.ID, "pat", r.label+" token", plain, &empID, nil)
		f.tokens[r.label] = plain
		f.empIDs[r.label] = empID
	}
	return f
}

func (f *payrollFixture) base() string { return "/api/v1/organizations/" + f.orgID }

// draftPayrollRun creates a draft run via the API and returns its id and
// version.
func draftPayrollRun(t *testing.T, f *payrollFixture, token, period, country string) (string, int64) {
	t.Helper()
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs", token,
		map[string]any{"period": period, "country_code": country}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create run %s: want 201, got %d: %s", period, rr.Code, rr.Body.String())
	}
	var run struct {
		ID              string `json:"id"`
		Status          string `json:"status"`
		Period          string `json:"period"`
		CountryCode     string `json:"country_code"`
		RulePackVersion string `json:"rule_pack_version"`
		Version         int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Status != "draft" {
		t.Fatalf("new run status = %q, want draft", run.Status)
	}
	wantPack := map[string]string{"MY": "my-2026-10", "ID": "id-2026-10"}[country]
	if run.RulePackVersion != wantPack {
		t.Fatalf("rule_pack_version = %q, want %q", run.RulePackVersion, wantPack)
	}
	return run.ID, run.Version
}

func calcBody(empID string, baseMinor int64, items []map[string]any) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{
		"employees": []map[string]any{
			{
				"employee_id":       empID,
				"base_salary_minor": baseMinor,
				"items":             items,
				"age_years":         30,
				"citizen_my":        true,
				"bank_account":      "1234567890",
				"bank_code":         "MBB",
			},
		},
	}
}

type payLineOut struct {
	ID              string `json:"id"`
	EmployeeID      string `json:"employee_id"`
	GrossMinor      int64  `json:"gross_minor"`
	AdditionsMinor  int64  `json:"additions_minor"`
	DeductionsMinor int64  `json:"deductions_minor"`
	NetMinor        int64  `json:"net_minor"`
	BankAccount     string `json:"bank_account"`
	Items           []struct {
		PresetCode  string `json:"preset_code"`
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		AmountMinor int64  `json:"amount_minor"`
		Note        string `json:"note"`
	} `json:"items"`
}

func calculateRun(t *testing.T, f *payrollFixture, token, runID string, body map[string]any) []payLineOut {
	t.Helper()
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/calculate", token, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("calculate: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		RunID string       `json:"run_id"`
		Lines []payLineOut `json:"lines"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode calculate: %v", err)
	}
	return out.Lines
}

func previewRun(t *testing.T, f *payrollFixture, token, runID string, body map[string]any) (string, []byte) {
	t.Helper()
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/preview", token, body, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		PreviewID string    `json:"preview_id"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	raw := append([]byte(nil), rr.Body.Bytes()...)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if out.PreviewID == "" {
		t.Fatalf("preview_id empty: %s", raw)
	}
	until := time.Until(out.ExpiresAt)
	if until < 25*time.Minute || until > 35*time.Minute {
		t.Fatalf("preview expiry in %v, want ~30m", until)
	}
	return out.PreviewID, raw
}

func submitRun(t *testing.T, f *payrollFixture, token, runID, previewID string, version int64, headers map[string]string) *http.Response {
	t.Helper()
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", token,
		map[string]any{"preview_id": previewID, "version": version}, headers)
	return rr.Result()
}

func lockRun(t *testing.T, f *payrollFixture, token, runID string, version int64) {
	t.Helper()
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/lock", token,
		map[string]any{"version": version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("lock: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var run struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode lock: %v", err)
	}
	if run.Status != "locked" {
		t.Fatalf("lock status = %q, want locked", run.Status)
	}
}

func payrollErrCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	return e.Code
}

// --- lifecycle ---

func TestPayrollRunLifecycle(t *testing.T) {
	f := newPayrollFixture(t, "lifecycle")

	// Duplicate period is rejected.
	runID, version := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs", f.tokens["owner"],
		map[string]any{"period": "2026-10", "country_code": "MY"}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "ALREADY_EXISTS" {
		t.Fatalf("duplicate period: want 409 ALREADY_EXISTS, got %d %s", rr.Code, rr.Body.String())
	}

	// Bad period / country.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs", f.tokens["owner"],
		map[string]any{"period": "2026-13", "country_code": "MY"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad period: want 400, got %d %s", rr.Code, rr.Body.String())
	}

	// List + get.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs?status=draft", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list runs: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID, f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get run: want 200, got %d %s", rr.Code, rr.Body.String())
	}

	// Calculate: two employees, overtime on one.
	body := map[string]any{
		"employees": []map[string]any{
			{
				"employee_id": f.empIDs["manager"], "base_salary_minor": int64(500000),
				"items":     []map[string]any{{"preset_code": "overtime", "amount_minor": int64(50000)}},
				"age_years": 30, "citizen_my": true,
				"bank_account": "1234567890", "bank_code": "MBB",
			},
			{
				"employee_id": f.empIDs["member"], "base_salary_minor": int64(300000),
				"items":     []map[string]any{},
				"age_years": 25, "citizen_my": true,
			},
		},
	}
	lines := calculateRun(t, f, f.tokens["owner"], runID, body)
	if len(lines) != 2 {
		t.Fatalf("calculate lines = %d, want 2", len(lines))
	}
	var mgr *payLineOut
	for i := range lines {
		if lines[i].EmployeeID == f.empIDs["manager"] {
			mgr = &lines[i]
		}
	}
	if mgr == nil {
		t.Fatal("no pay line for manager employee")
	}
	if mgr.GrossMinor != 550000 {
		t.Fatalf("gross = %d, want 550000", mgr.GrossMinor)
	}
	if mgr.AdditionsMinor != 50000 {
		t.Fatalf("additions = %d, want 50000", mgr.AdditionsMinor)
	}
	if mgr.NetMinor <= 0 || mgr.NetMinor >= mgr.GrossMinor {
		t.Fatalf("net = %d, want positive and below gross", mgr.NetMinor)
	}
	if mgr.BankAccount != "1234567890" {
		t.Fatalf("bank_account = %q, want 1234567890", mgr.BankAccount)
	}
	foundBasic, foundOT := false, false
	for _, it := range mgr.Items {
		if it.PresetCode == "basic_salary" && it.AmountMinor == 500000 {
			foundBasic = true
		}
		if it.PresetCode == "overtime" && it.AmountMinor == 50000 {
			foundOT = true
		}
	}
	if !foundBasic || !foundOT {
		t.Fatalf("items missing basic_salary/overtime: %+v", mgr.Items)
	}

	// Unknown preset code is a 400; unknown employee is a 404.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/calculate", f.tokens["owner"],
		map[string]any{"employees": []map[string]any{
			{"employee_id": f.empIDs["member"], "base_salary_minor": int64(300000),
				"items": []map[string]any{{"preset_code": "nope", "amount_minor": int64(1)}}},
		}}, nil)
	if rr.Code != http.StatusBadRequest || payrollErrCode(t, rr.Body.Bytes()) != "VALIDATION_ERROR" {
		t.Fatalf("unknown preset: want 400 VALIDATION_ERROR, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/calculate", f.tokens["owner"],
		map[string]any{"employees": []map[string]any{
			{"employee_id": "emp-does-not-exist", "base_salary_minor": int64(300000), "items": []map[string]any{}},
		}}, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown employee: want 404, got %d %s", rr.Code, rr.Body.String())
	}

	// Preview: dry run, NO_PRIOR_RUN on the first run, totals aggregated.
	previewID, raw := previewRun(t, f, f.tokens["owner"], runID, body)
	var pv struct {
		Employees []struct {
			EmployeeID string `json:"employee_id"`
			Anomalies  []struct {
				Code string `json:"code"`
			} `json:"anomalies"`
		} `json:"employees"`
		Totals struct {
			EmployeeCount int   `json:"employee_count"`
			GrossMinor    int64 `json:"gross_minor"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(raw, &pv); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if len(pv.Employees) != 2 {
		t.Fatalf("preview employees = %d, want 2", len(pv.Employees))
	}
	for _, e := range pv.Employees {
		found := false
		for _, a := range e.Anomalies {
			if a.Code == "NO_PRIOR_RUN" {
				found = true
			}
		}
		if !found {
			t.Fatalf("employee %s: want NO_PRIOR_RUN anomaly, got %+v", e.EmployeeID, e.Anomalies)
		}
	}
	if pv.Totals.EmployeeCount != 2 || pv.Totals.GrossMinor != 850000 {
		t.Fatalf("preview totals = %+v, want {2 850000}", pv.Totals)
	}

	// Preview persisted nothing: the stored lines are still the two from
	// calculate (no dupes on re-calculate either).
	lines2 := calculateRun(t, f, f.tokens["owner"], runID, body)
	if len(lines2) != 2 {
		t.Fatalf("re-calculate lines = %d, want 2", len(lines2))
	}

	// Submit with a bad version conflicts; then the real submit approves.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", f.tokens["owner"],
		map[string]any{"preview_id": previewID, "version": int64(999)}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "VERSION_CONFLICT" {
		t.Fatalf("submit bad version: want 409 VERSION_CONFLICT, got %d %s", rr.Code, rr.Body.String())
	}
	resp := submitRun(t, f, f.tokens["owner"], runID, previewID, version, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: want 200, got %d", resp.StatusCode)
	}
	var submitted struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&submitted); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	if submitted.Status != "approved" {
		t.Fatalf("submit status = %q, want approved", submitted.Status)
	}

	// Lock, then everything mutating is rejected on the locked run.
	lockRun(t, f, f.tokens["owner"], runID, submitted.Version)
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/calculate", f.tokens["owner"], body, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "RUN_NOT_DRAFT" {
		t.Fatalf("calculate on locked: want 409 RUN_NOT_DRAFT, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", f.tokens["owner"],
		map[string]any{"preview_id": previewID, "version": submitted.Version + 1}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "RUN_NOT_DRAFT" {
		t.Fatalf("submit on locked: want 409 RUN_NOT_DRAFT, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/cancel", f.tokens["owner"],
		map[string]any{"version": submitted.Version + 1}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "ILLEGAL_TRANSITION" {
		t.Fatalf("cancel on locked: want 409 ILLEGAL_TRANSITION, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/sync-items", f.tokens["owner"],
		map[string]any{"items": []any{}}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "RUN_NOT_DRAFT" {
		t.Fatalf("sync-items on locked: want 409 RUN_NOT_DRAFT, got %d %s", rr.Code, rr.Body.String())
	}

	// Audit trail recorded the lifecycle.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/audit_events?action=payroll.submitted", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("audit events: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var audits struct {
		Data []struct {
			Action string `json:"action"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &audits); err != nil {
		t.Fatalf("decode audits: %v", err)
	}
	if len(audits.Data) == 0 {
		t.Fatal("no payroll.submitted audit event recorded")
	}
}

// --- submit gate ---

func TestPayrollSubmitGate(t *testing.T) {
	f := newPayrollFixture(t, "gate")
	runID, version := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")
	body := calcBody(f.empIDs["member"], 300000, nil)
	calculateRun(t, f, f.tokens["owner"], runID, body)
	previewID, _ := previewRun(t, f, f.tokens["owner"], runID, body)

	// No preview id at all.
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", f.tokens["owner"],
		map[string]any{"preview_id": "", "version": version}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "PREVIEW_REQUIRED" {
		t.Fatalf("submit without preview: want 409 PREVIEW_REQUIRED, got %d %s", rr.Code, rr.Body.String())
	}

	// Unknown preview id.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", f.tokens["owner"],
		map[string]any{"preview_id": "pv-nope", "version": version}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "PREVIEW_STALE" {
		t.Fatalf("submit unknown preview: want 409 PREVIEW_STALE, got %d %s", rr.Code, rr.Body.String())
	}

	// Foreign preview: preview from another run is rejected.
	run2, _ := draftPayrollRun(t, f, f.tokens["owner"], "2026-11", "MY")
	foreignPreview, _ := previewRun(t, f, f.tokens["owner"], run2, calcBody(f.empIDs["member"], 300000, nil))
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+run2+"/submit", f.tokens["owner"],
		map[string]any{"preview_id": previewID, "version": int64(1)}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "PREVIEW_STALE" {
		t.Fatalf("submit foreign preview: want 409 PREVIEW_STALE, got %d %s", rr.Code, rr.Body.String())
	}

	// Expired preview reads as stale. Save one directly with a past
	// expiry through the store.
	ps := f.st.(store.PayrollStore)
	expired, err := ps.SavePayrollPreview(context.Background(), store.PayrollPreview{
		OrganizationID: f.orgID, RunID: runID,
		Payload:   json.RawMessage(`{"employees":[],"totals":{}}`),
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("save expired preview: %v", err)
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", f.tokens["owner"],
		map[string]any{"preview_id": expired.ID, "version": version}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "PREVIEW_STALE" {
		t.Fatalf("submit expired preview: want 409 PREVIEW_STALE, got %d %s", rr.Code, rr.Body.String())
	}

	// The foreign preview is valid for its own run.
	resp := submitRun(t, f, f.tokens["owner"], run2, foreignPreview, 1, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit run2 with own preview: want 200, got %d", resp.StatusCode)
	}
}

// --- idempotent submit ---

func TestPayrollSubmitIdempotent(t *testing.T) {
	f := newPayrollFixture(t, "idem")
	runID, version := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")
	body := calcBody(f.empIDs["member"], 300000, nil)
	calculateRun(t, f, f.tokens["owner"], runID, body)
	previewID, _ := previewRun(t, f, f.tokens["owner"], runID, body)

	h := map[string]string{"Idempotency-Key": "payroll-submit-1"}
	r1 := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", f.tokens["owner"],
		map[string]any{"preview_id": previewID, "version": version}, h)
	r2 := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", f.tokens["owner"],
		map[string]any{"preview_id": previewID, "version": version}, h)
	if r1.Code != http.StatusOK || r2.Code != http.StatusOK {
		t.Fatalf("idempotent submit: got %d / %d: %s / %s", r1.Code, r2.Code, r1.Body.String(), r2.Body.String())
	}
	if r1.Body.String() != r2.Body.String() {
		t.Fatalf("idempotent submit: bodies differ:\n%s\n%s", r1.Body.String(), r2.Body.String())
	}
	// The run transitioned exactly once: still approved, version 2.
	rr := doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID, f.tokens["owner"], nil, nil)
	var run struct {
		Status  string `json:"status"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Status != "approved" || run.Version != version+1 {
		t.Fatalf("run after idempotent submit = %+v, want approved version %d", run, version+1)
	}
}

// --- RBAC ---

func TestPayrollRBAC(t *testing.T) {
	f := newPayrollFixture(t, "rbac")
	runID, version := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")
	body := map[string]any{
		"employees": []map[string]any{
			{"employee_id": f.empIDs["member"], "base_salary_minor": int64(300000),
				"items": []map[string]any{}, "age_years": 25, "citizen_my": true},
			{"employee_id": f.empIDs["manager"], "base_salary_minor": int64(500000),
				"items": []map[string]any{}, "age_years": 30, "citizen_my": true},
		},
	}
	calculateRun(t, f, f.tokens["owner"], runID, body)

	// Members cannot draft runs; managers can.
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs", f.tokens["member"],
		map[string]any{"period": "2026-11", "country_code": "MY"}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member create run: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	draftPayrollRun(t, f, f.tokens["manager"], "2026-11", "MY")

	// Members cannot list runs or read presets; admins can. Managers list
	// via their role.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs", f.tokens["member"], nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member list runs: want 403, got %d", rr.Code)
	}
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs", f.tokens["manager"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("manager list runs: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-presets", f.tokens["admin"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin list presets: want 200, got %d %s", rr.Code, rr.Body.String())
	}

	// Submit needs payroll:submit scope or admin: member and manager are
	// both rejected.
	previewID, _ := previewRun(t, f, f.tokens["owner"], runID, body)
	for _, tok := range []string{f.tokens["member"], f.tokens["manager"]} {
		rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/submit", tok,
			map[string]any{"preview_id": previewID, "version": version}, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("non-admin submit: want 403, got %d %s", rr.Code, rr.Body.String())
		}
	}

	// Payslips: members read only their own.
	rr = doReq(t, f.srv, http.MethodGet,
		f.base()+"/payroll-runs/"+runID+"/payslips/"+f.empIDs["member"], f.tokens["member"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("member own payslip: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet,
		f.base()+"/payroll-runs/"+runID+"/payslips/"+f.empIDs["manager"], f.tokens["member"], nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member other payslip: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet,
		f.base()+"/payroll-runs/"+runID+"/payslips/"+f.empIDs["manager"], f.tokens["admin"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin any payslip: want 200, got %d %s", rr.Code, rr.Body.String())
	}

	// Bank files need manager or above.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID+"/bank-file", f.tokens["member"], nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member bank file: want 403, got %d %s", rr.Code, rr.Body.String())
	}

	// Cross-tenant isolation: the other org's token is rejected outright.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID, f.crossToken, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant get run: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs", f.crossToken,
		map[string]any{"period": "2026-10", "country_code": "MY"}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant create run: want 403, got %d %s", rr.Code, rr.Body.String())
	}

	// Unauthenticated.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs", "", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: want 401, got %d", rr.Code)
	}
}

// --- service-account scopes ---

func TestPayrollServiceAccountScopes(t *testing.T) {
	f := newPayrollFixture(t, "svcacct")
	mintToken(t, f.st, f.orgID, string(store.CredentialServiceAccount), "reporting",
		"svc-payroll-read-1", nil, []string{store.ScopePayrollRead})
	svc := "svc-payroll-read-1"

	runID := approvedLockedRun(t, f, "2026-10", "MY")

	// A service account holding payroll:read passes the scope leg of the
	// manager-level read gates.
	rr := doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs", svc, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("svc list runs: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID, svc, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("svc get run: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID+"/bank-file", svc, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("svc bank file: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID+"/exports/epf-csv", svc, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("svc epf-csv: want 200, got %d %s", rr.Code, rr.Body.String())
	}

	// But it cannot touch the write gates without the matching scopes.
	draftID, _ := draftPayrollRun(t, f, f.tokens["owner"], "2026-11", "MY")
	body := calcBody(f.empIDs["member"], 300000, nil)
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+draftID+"/preview", svc, body, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("svc preview without payroll:preview: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+draftID+"/submit", svc,
		map[string]any{"preview_id": "x", "version": int64(1)}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("svc submit without payroll:submit: want 403, got %d %s", rr.Code, rr.Body.String())
	}

	// And member PATs stay confined to their own payslip even though they
	// now hold payroll:read.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID+"/bank-file", f.tokens["member"], nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member bank file: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet,
		f.base()+"/payroll-runs/"+runID+"/payslips/"+f.empIDs["member"], f.tokens["member"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("member own payslip: want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

// --- unpaid leave auto-deduction ---

func TestPayrollUnpaidLeaveDeduction(t *testing.T) {
	f := newPayrollFixture(t, "unpaid")
	runID, _ := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")
	empID := f.empIDs["member"]

	// Policy + unpaid request Oct 6-8 (3 days), approved by the owner.
	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/leave-policies", f.tokens["owner"],
		map[string]any{"name": "Annual", "accrual_mode": "monthly", "accrual_amount": 1.25}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create policy: want 201, got %d %s", rr.Code, rr.Body.String())
	}
	var pol struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &pol); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/leave-requests", f.tokens["owner"],
		map[string]any{
			"employee_id": empID, "policy_id": pol.ID,
			"start_date": "2026-10-06", "end_date": "2026-10-08", "is_unpaid": true,
		}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create unpaid request: want 201, got %d %s", rr.Code, rr.Body.String())
	}
	var lr struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &lr); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/leave-requests/"+lr.ID+"/approve", f.tokens["owner"],
		map[string]any{"version": lr.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve unpaid request: want 200, got %d %s", rr.Code, rr.Body.String())
	}

	// A request outside the run month must not leak in.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/leave-requests", f.tokens["owner"],
		map[string]any{
			"employee_id": empID, "policy_id": pol.ID,
			"start_date": "2026-09-28", "end_date": "2026-09-30", "is_unpaid": true,
		}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create september request: want 201, got %d %s", rr.Code, rr.Body.String())
	}
	var lr2 struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &lr2); err != nil {
		t.Fatalf("decode request 2: %v", err)
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/leave-requests/"+lr2.ID+"/approve", f.tokens["owner"],
		map[string]any{"version": lr2.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("approve september request: want 200, got %d %s", rr.Code, rr.Body.String())
	}

	// base 520000 / 26 = 20000/day exactly; 3 unpaid days -> 60000.
	lines := calculateRun(t, f, f.tokens["owner"], runID, calcBody(empID, 520000, nil))
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var ded *struct {
		PresetCode  string `json:"preset_code"`
		AmountMinor int64  `json:"amount_minor"`
		Note        string `json:"note"`
	}
	for i := range lines[0].Items {
		it := lines[0].Items[i]
		if it.PresetCode == "unpaid_leave_deduction" {
			ded = &struct {
				PresetCode  string `json:"preset_code"`
				AmountMinor int64  `json:"amount_minor"`
				Note        string `json:"note"`
			}{it.PresetCode, it.AmountMinor, it.Note}
		}
	}
	if ded == nil {
		t.Fatalf("no unpaid_leave_deduction item: %+v", lines[0].Items)
	}
	want := int64(math.Round(3 * float64(520000/26)))
	if ded.AmountMinor != want {
		t.Fatalf("unpaid deduction = %d, want %d (3 days x 520000/26)", ded.AmountMinor, want)
	}
	if !strings.Contains(ded.Note, "3.00") {
		t.Fatalf("unpaid deduction note = %q, want it to mention 3.00 days", ded.Note)
	}
}

// --- sync items ---

func TestPayrollSyncItems(t *testing.T) {
	f := newPayrollFixture(t, "sync")
	runID, _ := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")
	empID := f.empIDs["member"]

	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/sync-items", f.tokens["owner"],
		map[string]any{"items": []map[string]any{
			{"employee_id": empID, "source": "overtime-svc", "external_id": "ot-1",
				"label": "OT week 1", "kind": "addition", "preset_code": "overtime", "amount_minor": int64(25000)},
			{"employee_id": empID, "source": "claims", "external_id": "adv-1",
				"label": "Advance", "kind": "deduction", "preset_code": "advance_payment", "amount_minor": int64(10000)},
		}}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("add sync items: want 201, got %d %s", rr.Code, rr.Body.String())
	}

	// Unknown preset code is rejected.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/sync-items", f.tokens["owner"],
		map[string]any{"items": []map[string]any{
			{"employee_id": empID, "kind": "addition", "preset_code": "nope", "amount_minor": int64(1)},
		}}, nil)
	if rr.Code != http.StatusBadRequest || payrollErrCode(t, rr.Body.Bytes()) != "VALIDATION_ERROR" {
		t.Fatalf("bad sync preset: want 400 VALIDATION_ERROR, got %d %s", rr.Code, rr.Body.String())
	}

	// The staged items fold into the next calculate: overtime 50000 (body)
	// + 25000 (sync), and the 10000 advance deduction.
	lines := calculateRun(t, f, f.tokens["owner"], runID,
		calcBody(empID, 300000, []map[string]any{{"preset_code": "overtime", "amount_minor": int64(50000)}}))
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(lines))
	}
	var otTotal, advTotal int64
	for _, it := range lines[0].Items {
		switch it.PresetCode {
		case "overtime":
			otTotal += it.AmountMinor
		case "advance_payment":
			advTotal += it.AmountMinor
		}
	}
	if otTotal != 75000 {
		t.Fatalf("overtime total = %d, want 75000 (50000 body + 25000 sync)", otTotal)
	}
	if advTotal != 10000 {
		t.Fatalf("advance total = %d, want 10000", advTotal)
	}
}

// --- presets ---

func TestPayrollPresets(t *testing.T) {
	f := newPayrollFixture(t, "presets")

	// The seed catalog is listed with IsSystem markers.
	rr := doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-presets", f.tokens["admin"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list presets: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var list struct {
		Data []struct {
			ID       string `json:"id"`
			Code     string `json:"code"`
			IsSystem bool   `json:"is_system"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode presets: %v", err)
	}
	var sysID string
	for _, p := range list.Data {
		if p.Code == "basic_salary" {
			if !p.IsSystem {
				t.Fatal("basic_salary should be a system preset")
			}
			sysID = p.ID
		}
	}
	if sysID == "" {
		t.Fatal("basic_salary not in preset catalog")
	}

	// System presets are immutable via the API.
	rr = doReq(t, f.srv, http.MethodPatch, f.base()+"/payroll-presets/"+sysID, f.tokens["admin"],
		map[string]any{"name": "Hacked", "version": int64(1)}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "SYSTEM_PRESET_IMMUTABLE" {
		t.Fatalf("patch system preset: want 409 SYSTEM_PRESET_IMMUTABLE, got %d %s", rr.Code, rr.Body.String())
	}

	// Managers cannot manage presets.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-presets", f.tokens["manager"],
		map[string]any{"code": "x", "name": "x", "kind": "addition"}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("manager create preset: want 403, got %d %s", rr.Code, rr.Body.String())
	}

	// Create an org preset, collide with a seed code, then patch it.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-presets", f.tokens["admin"],
		map[string]any{
			"code": "meal_allowance", "name": "Meal Allowance", "kind": "addition",
			"subject_to_epf": true, "subject_to_socso": true,
		}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create preset: want 201, got %d %s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID       string `json:"id"`
		Code     string `json:"code"`
		IsSystem bool   `json:"is_system"`
		Version  int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created preset: %v", err)
	}
	if created.IsSystem {
		t.Fatal("org preset marked as system")
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-presets", f.tokens["admin"],
		map[string]any{"code": "overtime", "name": "Dupe", "kind": "addition"}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "ALREADY_EXISTS" {
		t.Fatalf("seed code collision: want 409 ALREADY_EXISTS, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPatch, f.base()+"/payroll-presets/"+created.ID, f.tokens["admin"],
		map[string]any{"name": "Meal Allowance (updated)", "subject_to_pcb": true, "version": created.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch preset: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var updated struct {
		Name         string `json:"name"`
		SubjectToPCB bool   `json:"subject_to_pcb"`
		Version      int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated preset: %v", err)
	}
	if updated.Name != "Meal Allowance (updated)" || !updated.SubjectToPCB {
		t.Fatalf("patch not applied: %+v", updated)
	}
	rr = doReq(t, f.srv, http.MethodPatch, f.base()+"/payroll-presets/"+created.ID, f.tokens["admin"],
		map[string]any{"name": "Stale", "version": created.Version}, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "VERSION_CONFLICT" {
		t.Fatalf("stale patch: want 409 VERSION_CONFLICT, got %d %s", rr.Code, rr.Body.String())
	}
}

// --- payslips, bank files, exports ---

func approvedLockedRun(t *testing.T, f *payrollFixture, period, country string) string {
	t.Helper()
	runID, version := draftPayrollRun(t, f, f.tokens["owner"], period, country)
	body := calcBody(f.empIDs["member"], 500000, nil)
	calculateRun(t, f, f.tokens["owner"], runID, body)
	previewID, _ := previewRun(t, f, f.tokens["owner"], runID, body)
	resp := submitRun(t, f, f.tokens["owner"], runID, previewID, version, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submit: want 200, got %d", resp.StatusCode)
	}
	var submitted struct {
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&submitted); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	lockRun(t, f, f.tokens["owner"], runID, submitted.Version)
	return runID
}

func TestPayrollExports(t *testing.T) {
	f := newPayrollFixture(t, "exports")
	runID := approvedLockedRun(t, f, "2026-10", "MY")

	// Payslip JSON + HTML.
	rr := doReq(t, f.srv, http.MethodGet,
		f.base()+"/payroll-runs/"+runID+"/payslips/"+f.empIDs["member"], f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("payslip json: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var line payLineOut
	if err := json.Unmarshal(rr.Body.Bytes(), &line); err != nil {
		t.Fatalf("decode payslip: %v", err)
	}
	if line.EmployeeID != f.empIDs["member"] || line.NetMinor <= 0 {
		t.Fatalf("payslip line = %+v, want member line with positive net", line)
	}
	rr = doReq(t, f.srv, http.MethodGet,
		f.base()+"/payroll-runs/"+runID+"/payslips/"+f.empIDs["member"]+"?format=html", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("payslip html: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("payslip html content-type = %q", ct)
	}
	if !strings.Contains(rr.Body.String(), "2026-10") {
		t.Fatalf("payslip html missing period: %.200s", rr.Body.String())
	}

	// Bank files: pipe + maybank, attachment disposition.
	for _, format := range []string{"pipe", "maybank"} {
		rr = doReq(t, f.srv, http.MethodGet,
			f.base()+"/payroll-runs/"+runID+"/bank-file?format="+format, f.tokens["owner"], nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("bank-file %s: want 200, got %d %s", format, rr.Code, rr.Body.String())
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("bank-file %s content-type = %q", format, ct)
		}
		if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Fatalf("bank-file %s disposition = %q, want attachment", format, cd)
		}
		if !strings.Contains(rr.Body.String(), "1234567890") {
			t.Fatalf("bank-file %s missing bank account: %.200s", format, rr.Body.String())
		}
	}
	rr = doReq(t, f.srv, http.MethodGet,
		f.base()+"/payroll-runs/"+runID+"/bank-file?format=nope", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bank-file bad format: want 400, got %d", rr.Code)
	}

	// Statutory exports.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID+"/exports/epf-csv", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("epf-csv: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if !strings.HasPrefix(rr.Body.String(), "period,employee_id") {
		t.Fatalf("epf-csv header wrong: %.100s", rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID+"/exports/pcb-txt", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("pcb-txt: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+runID+"/exports/borang-a", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("borang-a: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "BORANG A") {
		t.Fatalf("borang-a body wrong: %.200s", rr.Body.String())
	}

	// Draft runs cannot generate files.
	draftID, _ := draftPayrollRun(t, f, f.tokens["owner"], "2026-11", "MY")
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+draftID+"/bank-file", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "RUN_NOT_APPROVED" {
		t.Fatalf("bank-file on draft: want 409 RUN_NOT_APPROVED, got %d %s", rr.Code, rr.Body.String())
	}

	// MY-only exports reject ID runs.
	idRunID, idVersion := draftPayrollRun(t, f, f.tokens["owner"], "2026-12", "ID")
	ps := f.st.(store.PayrollStore)
	if _, err := ps.TransitionPayrollRun(context.Background(), f.orgID, idRunID, idVersion, store.PayrollRunApproved, "test"); err != nil {
		t.Fatalf("approve ID run: %v", err)
	}
	for _, path := range []string{"exports/epf-csv", "exports/pcb-txt", "exports/borang-a"} {
		rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+idRunID+"/"+path, f.tokens["owner"], nil, nil)
		if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "WRONG_COUNTRY" {
			t.Fatalf("%s on ID run: want 409 WRONG_COUNTRY, got %d %s", path, rr.Code, rr.Body.String())
		}
	}
	// But the bank file is country-agnostic.
	rr = doReq(t, f.srv, http.MethodGet, f.base()+"/payroll-runs/"+idRunID+"/bank-file", f.tokens["owner"], nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bank-file on ID run: want 200, got %d %s", rr.Code, rr.Body.String())
	}
}

// --- preview diff against a prior run ---

func TestPayrollPreviewDiff(t *testing.T) {
	f := newPayrollFixture(t, "diff")
	empID := f.empIDs["member"]

	// October run, submitted and locked: becomes the "previous" run.
	octID, octVersion := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")
	octBody := calcBody(empID, 500000, nil)
	octLines := calculateRun(t, f, f.tokens["owner"], octID, octBody)
	octPreview, _ := previewRun(t, f, f.tokens["owner"], octID, octBody)
	resp := submitRun(t, f, f.tokens["owner"], octID, octPreview, octVersion, nil)
	var octSubmitted struct {
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&octSubmitted); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	lockRun(t, f, f.tokens["owner"], octID, octSubmitted.Version)

	// November run with a raise: the diff must reference October's net and
	// flag the >20% swing.
	novID, _ := draftPayrollRun(t, f, f.tokens["owner"], "2026-11", "MY")
	novBody := calcBody(empID, 700000, nil)
	_, raw := previewRun(t, f, f.tokens["owner"], novID, novBody)
	var pv struct {
		Employees []struct {
			EmployeeID   string  `json:"employee_id"`
			PrevNetMinor int64   `json:"prev_net_minor"`
			NewNetMinor  int64   `json:"new_net_minor"`
			DiffPct      float64 `json:"diff_pct"`
			Anomalies    []struct {
				Code string `json:"code"`
			} `json:"anomalies"`
		} `json:"employees"`
	}
	if err := json.Unmarshal(raw, &pv); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if len(pv.Employees) != 1 {
		t.Fatalf("preview employees = %d, want 1", len(pv.Employees))
	}
	e := pv.Employees[0]
	if e.PrevNetMinor != octLines[0].NetMinor {
		t.Fatalf("prev net = %d, want october net %d", e.PrevNetMinor, octLines[0].NetMinor)
	}
	if e.NewNetMinor <= e.PrevNetMinor {
		t.Fatalf("new net = %d, want above prev %d", e.NewNetMinor, e.PrevNetMinor)
	}
	found := false
	for _, a := range e.Anomalies {
		if a.Code == "NET_SWING_GT_20" {
			found = true
		}
		if a.Code == "NO_PRIOR_RUN" {
			t.Fatal("NO_PRIOR_RUN must not appear when a prior run exists")
		}
	}
	if !found {
		t.Fatalf("want NET_SWING_GT_20 anomaly (40%% raise), got %+v", e.Anomalies)
	}
}

// --- cancel ---

func TestPayrollCancel(t *testing.T) {
	f := newPayrollFixture(t, "cancel")
	runID, version := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")

	rr := doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/cancel", f.tokens["manager"],
		map[string]any{"version": version}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("manager cancel: want 403, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/cancel", f.tokens["owner"],
		map[string]any{"version": version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("cancel draft: want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var cancelled struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &cancelled); err != nil {
		t.Fatalf("decode cancel: %v", err)
	}
	if cancelled.Status != "cancelled" {
		t.Fatalf("cancel status = %q", cancelled.Status)
	}
	// A cancelled run is no longer draft: calculate is rejected, and the
	// cancelled run is skipped as a diff baseline.
	rr = doReq(t, f.srv, http.MethodPost, f.base()+"/payroll-runs/"+runID+"/calculate", f.tokens["owner"],
		calcBody(f.empIDs["member"], 300000, nil), nil)
	if rr.Code != http.StatusConflict || payrollErrCode(t, rr.Body.Bytes()) != "RUN_NOT_DRAFT" {
		t.Fatalf("calculate on cancelled: want 409 RUN_NOT_DRAFT, got %d %s", rr.Code, rr.Body.String())
	}
}

// TestPayrollPreviewPersistsLines pins the unified preview semantics:
// REST preview persists the computed lines on the draft run, exactly like
// the MCP preview — both surfaces share persistPayLines, so they never
// drift. Previewing twice with different inputs updates (never duplicates)
// the lines.
func TestPayrollPreviewPersistsLines(t *testing.T) {
	f := newPayrollFixture(t, "previewlines")
	empID := f.empIDs["member"]
	runID, _ := draftPayrollRun(t, f, f.tokens["owner"], "2026-10", "MY")

	getLines := func() []store.EmployeePayLine {
		t.Helper()
		ps, ok := f.st.(store.PayrollStore)
		if !ok {
			t.Fatalf("store does not implement PayrollStore")
		}
		lines, _, err := ps.ListPayLines(context.Background(), f.orgID, runID, store.Page{Limit: 1000})
		if err != nil {
			t.Fatalf("ListPayLines: %v", err)
		}
		return lines
	}

	// Preview without a prior calculate: lines must still be persisted.
	previewRun(t, f, f.tokens["owner"], runID, calcBody(empID, 500000, nil))
	lines := getLines()
	if len(lines) != 1 {
		t.Fatalf("lines after preview = %d, want 1", len(lines))
	}
	if lines[0].NetMinor <= 0 {
		t.Fatalf("net = %d, want positive", lines[0].NetMinor)
	}
	firstNet := lines[0].NetMinor

	// Re-preview with a raise: the single line is updated, not duplicated.
	previewRun(t, f, f.tokens["owner"], runID, calcBody(empID, 700000, nil))
	lines = getLines()
	if len(lines) != 1 {
		t.Fatalf("lines after re-preview = %d, want 1", len(lines))
	}
	if lines[0].NetMinor <= firstNet {
		t.Fatalf("net after raise = %d, want above %d", lines[0].NetMinor, firstNet)
	}
}
