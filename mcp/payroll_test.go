package mcp

// Payroll MCP tool tests (component 5): registry shape, the draft ->
// preview -> submit two-phase gate, tier/scope/role gates, member
// payslip scoping, and the preview-match rejections.

import (
	"context"
	"testing"

	"github.com/azmi2409/forge-hris/store"
)

// payrollFixture builds an admin authed session plus two joined employees
// the pay lines can reference.
func payrollFixture(t *testing.T) (*Server, store.Store, *AuthInfo, string, string, string) {
	t.Helper()
	srv, st, ai, orgID := adminFixture(t)
	empA := joinedEmployee(t, st, orgID, "E-PAY-1", mcpUniq("paya")+"@t.test", store.RoleMember)
	empB := joinedEmployee(t, st, orgID, "E-PAY-2", mcpUniq("payb")+"@t.test", store.RoleMember)
	return srv, st, ai, orgID, empA, empB
}

func payrollEmpArg(empID string, salaryMinor float64) map[string]any {
	return map[string]any{
		"employee_id":       empID,
		"base_salary_minor": salaryMinor,
		"age_years":         float64(30),
		"citizen_my":        true,
		"bank_account":      "1234567890",
		"bank_code":         "MBB",
	}
}

func draftRun(t *testing.T, srv *Server, ai *AuthInfo, period, country string) map[string]any {
	t.Helper()
	res := mustCall(t, srv, ai, "hris.draft_payroll_run", map[string]any{
		"period": period, "country_code": country,
	})
	return resultJSON(t, res)["payroll_run"].(map[string]any)
}

func previewRun(t *testing.T, srv *Server, ai *AuthInfo, runID string, emps ...map[string]any) map[string]any {
	t.Helper()
	anyEmps := make([]any, 0, len(emps))
	for _, e := range emps {
		anyEmps = append(anyEmps, e)
	}
	res := mustCall(t, srv, ai, "hris.preview_payroll", map[string]any{
		"run_id": runID, "employees": anyEmps,
	})
	if res.IsError {
		t.Fatalf("preview_payroll failed: %s", res.Content[0].Text)
	}
	return resultJSON(t, res)
}

func TestPayrollToolRegistry(t *testing.T) {
	srv, _ := newTestMCP(t)
	reg := srv.Registry()
	cases := []struct {
		name  string
		tier  Tier
		scope string
		role  store.Role
	}{
		{"hris.list_payroll_runs", TierAuto, store.ScopePayrollRead, ""},
		{"hris.get_payroll_run", TierAuto, store.ScopePayrollRead, ""},
		{"hris.draft_payroll_run", TierAuto, store.ScopePayrollPreview, ""},
		{"hris.preview_payroll", TierAuto, store.ScopePayrollPreview, ""},
		{"hris.submit_payroll", TierNever, store.ScopePayrollSubmit, store.RoleAdmin},
		{"hris.cancel_payroll_run", TierConfirm, store.ScopePayrollSubmit, store.RoleAdmin},
		{"hris.get_payslip", TierAuto, store.ScopePayrollRead, ""},
		{"hris.list_pay_schedules", TierAuto, store.ScopePayrollRead, ""},
	}
	for _, c := range cases {
		def, ok := reg.Get(c.name)
		if !ok {
			t.Errorf("tool %s not registered", c.name)
			continue
		}
		if def.Tier != c.tier {
			t.Errorf("%s tier = %q, want %q", c.name, def.Tier, c.tier)
		}
		if def.RequiredScope != c.scope {
			t.Errorf("%s scope = %q, want %q", c.name, def.RequiredScope, c.scope)
		}
		if def.MinRole != c.role {
			t.Errorf("%s minRole = %q, want %q", c.name, def.MinRole, c.role)
		}
		if (c.tier == TierConfirm || c.tier == TierNever) && def.Preview == nil {
			t.Errorf("%s is gated but has no Preview func", c.name)
		}
	}
}

func TestDraftPayrollRunValidation(t *testing.T) {
	srv, _, ai, _ := adminFixture(t)

	res := mustCall(t, srv, ai, "hris.draft_payroll_run",
		map[string]any{"period": "2026-13", "country_code": "MY"})
	if got := toolErrCode(t, res); got != "VALIDATION_ERROR" {
		t.Errorf("bad period: code = %q, want VALIDATION_ERROR", got)
	}

	res = mustCall(t, srv, ai, "hris.draft_payroll_run",
		map[string]any{"period": "2026-10", "country_code": "SG"})
	if got := toolErrCode(t, res); got != "VALIDATION_ERROR" {
		t.Errorf("bad country: code = %q, want VALIDATION_ERROR", got)
	}

	draftRun(t, srv, ai, "2026-10", "MY")
	res = mustCall(t, srv, ai, "hris.draft_payroll_run",
		map[string]any{"period": "2026-10", "country_code": "MY"})
	if got := toolErrCode(t, res); got != "ALREADY_EXISTS" {
		t.Errorf("duplicate period: code = %q, want ALREADY_EXISTS", got)
	}
}

func TestPreviewPayrollHappyPath(t *testing.T) {
	srv, st, ai, orgID, empA, empB := payrollFixture(t)
	run := draftRun(t, srv, ai, "2026-10", "MY")
	runID, _ := run["id"].(string)

	out := previewRun(t, srv, ai, runID,
		payrollEmpArg(empA, 500000), payrollEmpArg(empB, 800000))
	if out["preview_id"] == "" {
		t.Fatal("preview_id is empty")
	}
	totals, _ := out["totals"].([]any)
	if len(totals) != 2 {
		t.Fatalf("totals has %d rows, want 2", len(totals))
	}
	for _, raw := range totals {
		row := raw.(map[string]any)
		if row["net_minor"].(float64) <= 0 {
			t.Errorf("employee %v has non-positive net", row["employee_id"])
		}
	}
	// No prior run exists: every employee must carry NO_PRIOR_RUN.
	// (payroll.Anomaly has no JSON tags, so keys are Go-cased.)
	anoms, _ := out["anomalies"].([]any)
	found := false
	for _, raw := range anoms {
		if raw.(map[string]any)["code"] == "NO_PRIOR_RUN" {
			found = true
		}
	}
	if !found {
		t.Errorf("want a NO_PRIOR_RUN anomaly, got %v", out["anomalies"])
	}
	if out["anomaly_count"].(float64) != float64(len(anoms)) {
		t.Errorf("anomaly_count mismatch")
	}

	// Lines were persisted on the draft run.
	res := mustCall(t, srv, ai, "hris.get_payroll_run",
		map[string]any{"run_id": runID, "lines": true})
	got := resultJSON(t, res)
	if lines, _ := got["pay_lines"].([]any); len(lines) != 2 {
		t.Fatalf("pay_lines has %d rows, want 2", len(lines))
	}

	// Re-previewing the same run updates the lines instead of failing.
	out2 := previewRun(t, srv, ai, runID,
		payrollEmpArg(empA, 500000), payrollEmpArg(empB, 800000))
	if out2["preview_id"] == "" {
		t.Fatal("second preview_id is empty")
	}

	if !containsAction(agentAuditActions(t, st, orgID), "payroll_run.calculated") {
		t.Error("missing payroll_run.calculated audit event")
	}
}

func TestSubmitPayrollNeverTier(t *testing.T) {
	srv, st, ai, orgID, empA, empB := payrollFixture(t)
	ctx := context.Background()

	// A wildcard subscription so the webhook fan-out is observable.
	if _, err := st.CreateWebhookSubscription(ctx, store.WebhookSubscription{
		OrganizationID: orgID,
		URL:            "https://example.com/hook",
		Secret:         "s3cret",
		Events:         []string{"*"},
		Active:         true,
	}); err != nil {
		t.Fatalf("CreateWebhookSubscription: %v", err)
	}

	run := draftRun(t, srv, ai, "2026-10", "MY")
	runID, _ := run["id"].(string)
	version := run["version"].(float64)
	out := previewRun(t, srv, ai, runID,
		payrollEmpArg(empA, 500000), payrollEmpArg(empB, 800000))
	previewID, _ := out["preview_id"].(string)
	args := map[string]any{"run_id": runID, "preview_id": previewID, "version": version}

	t.Run("first call returns preview and executes nothing", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.submit_payroll", args)
		if res.IsError {
			t.Fatalf("want approval_required, got error: %s", res.Content[0].Text)
		}
		got := resultJSON(t, res)
		if got["approval_required"] != true {
			t.Fatalf("approval_required != true: %v", got)
		}
		if got["tier"] != "never" {
			t.Errorf("tier = %v, want never", got["tier"])
		}
		pv, _ := got["preview"].(map[string]any)
		if pv["anomaly_count"] == nil || pv["totals"] == nil {
			t.Errorf("preview missing totals/anomaly_count: %v", pv)
		}
		still, err := st.GetPayrollRun(ctx, orgID, runID)
		if err != nil {
			t.Fatal(err)
		}
		if still.Status != store.PayrollRunDraft {
			t.Errorf("run status = %q after unapproved call, want draft", still.Status)
		}
		if containsAction(agentAuditActions(t, st, orgID), "payroll.submitted") {
			t.Error("audit event written before confirmation")
		}

		t.Run("resume without confirmed:true is rejected", func(t *testing.T) {
			token, _ := got["resume_token"].(string)
			res := mustCall(t, srv, ai, "hris.submit_payroll",
				map[string]any{"resume_token": token})
			if code := toolErrCode(t, res); code != "APPROVAL_NOT_CONFIRMED" {
				t.Errorf("code = %q, want APPROVAL_NOT_CONFIRMED", code)
			}
		})

		t.Run("resume with token and confirmed:true executes", func(t *testing.T) {
			// Fresh token: the previous subtest consumed the first one.
			res := mustCall(t, srv, ai, "hris.submit_payroll", args)
			token, _ := resultJSON(t, res)["resume_token"].(string)
			res = mustCall(t, srv, ai, "hris.submit_payroll",
				map[string]any{"resume_token": token, "confirmed": true})
			if res.IsError {
				t.Fatalf("submit failed: %s", res.Content[0].Text)
			}
			done := resultJSON(t, res)
			if done["payslips_issued"].(float64) != 2 {
				t.Errorf("payslips_issued = %v, want 2", done["payslips_issued"])
			}
			final, err := st.GetPayrollRun(ctx, orgID, runID)
			if err != nil {
				t.Fatal(err)
			}
			if final.Status != store.PayrollRunApproved {
				t.Errorf("run status = %q, want approved", final.Status)
			}
			if !containsAction(agentAuditActions(t, st, orgID), "payroll.submitted") {
				t.Error("missing payroll.submitted audit event")
			}
			// One payroll.submitted + one payslip.issued per pay line.
			msgs, err := st.ClaimWebhookDeliveries(ctx, 100)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[string]int{}
			for _, m := range msgs {
				counts[m.EventType]++
			}
			if counts["payroll.submitted"] != 1 {
				t.Errorf("payroll.submitted events = %d, want 1 (%v)", counts["payroll.submitted"], counts)
			}
			if counts["payslip.issued"] != 2 {
				t.Errorf("payslip.issued events = %d, want 2 (%v)", counts["payslip.issued"], counts)
			}
		})
	})

	t.Run("submitting a non-draft run is rejected", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.submit_payroll", args)
		if code := toolErrCode(t, res); code != "RUN_NOT_DRAFT" {
			t.Errorf("code = %q, want RUN_NOT_DRAFT", code)
		}
	})
}

func TestSubmitPayrollScopeGate(t *testing.T) {
	srv, st, _, orgID := adminFixture(t)
	mintToken(t, st, orgID, "service_account", "ro", "tok-ro", nil,
		[]string{store.ScopePayrollRead})
	ai := mustAuth(t, srv, "tok-ro")
	_, rerr := srv.executeTool(context.Background(), ai, "hris.submit_payroll",
		map[string]any{"run_id": "x", "preview_id": "y", "version": float64(1)})
	if rerr == nil || rerr.Code != rpcForbidden {
		t.Fatalf("want forbidden, got %+v", rerr)
	}
}

func TestSubmitPayrollMinRoleGate(t *testing.T) {
	srv, st, _, orgID := adminFixture(t)
	// Manager scopes plus payroll:submit: passes the scope gate (effective
	// role resolves to manager) but fails MinRole admin.
	scopes := append(append([]string(nil), scopesForRole(store.RoleManager)...), store.ScopePayrollSubmit)
	mintToken(t, st, orgID, "service_account", "mgrlike", "tok-mgrlike", nil, scopes)
	ai := mustAuth(t, srv, "tok-mgrlike")
	if got := effectiveRole(ai); got != store.RoleManager {
		t.Fatalf("effectiveRole = %q, want manager", got)
	}
	_, rerr := srv.executeTool(context.Background(), ai, "hris.submit_payroll",
		map[string]any{"run_id": "x", "preview_id": "y", "version": float64(1)})
	if rerr == nil || rerr.Code != rpcForbidden {
		t.Fatalf("want forbidden, got %+v", rerr)
	}
}

func TestGetPayslipMemberSelfOnly(t *testing.T) {
	srv, st, adminAI, orgID, empA, empB := payrollFixture(t)
	run := draftRun(t, srv, adminAI, "2026-10", "MY")
	runID, _ := run["id"].(string)
	previewRun(t, srv, adminAI, runID,
		payrollEmpArg(empA, 500000), payrollEmpArg(empB, 800000))

	ctx := context.Background()
	// Members don't hold payroll:read in the default scope map, so the
	// self-check is exercised at the handler level with a member-shaped
	// AuthInfo (member role + payroll:read scope + employee identity),
	// mirroring handleGetEmployee's own-only rule.
	memAI := &AuthInfo{
		OrgID: orgID, CredentialID: "cred-mem", Kind: store.CredentialPAT,
		EmployeeID: &empA, Role: store.RoleMember,
		Scopes: []string{store.ScopePayrollRead},
	}

	result, audit, terr := srv.handleGetPayslip(ctx, memAI,
		map[string]any{"run_id": runID, "employee_id": empA})
	if terr != nil {
		t.Fatalf("own payslip: unexpected error %v", terr)
	}
	if audit == nil || audit.action != "payslip.read" {
		t.Errorf("want payslip.read audit record, got %+v", audit)
	}
	slip, _ := result.(map[string]any)["payslip"].(store.EmployeePayLine)
	if slip.EmployeeID != empA {
		t.Errorf("payslip employee = %q, want %s", slip.EmployeeID, empA)
	}

	_, _, terr = srv.handleGetPayslip(ctx, memAI,
		map[string]any{"run_id": runID, "employee_id": empB})
	if terr == nil || terr.Code != "FORBIDDEN" {
		t.Errorf("other employee's payslip: want FORBIDDEN, got %v", terr)
	}

	// A real member PAT now holds payroll:read by default (own payslip only);
	// the handler's self-check is what stops cross-employee reads. Handler
	// ToolErrors surface as error results (not rpc errors) via executeTool.
	mintToken(t, st, orgID, "pat", "mem-a", "tok-mem-a", &empA, nil)
	patAI := mustAuth(t, srv, "tok-mem-a")
	res, rerr := srv.executeTool(ctx, patAI, "hris.get_payslip",
		map[string]any{"run_id": runID, "employee_id": empA})
	if rerr != nil {
		t.Fatalf("member PAT own payslip: unexpected rpc error %+v", rerr)
	}
	if res.IsError {
		t.Fatalf("member PAT own payslip: unexpected tool error: %s", res.Content[0].Text)
	}
	res, rerr = srv.executeTool(ctx, patAI, "hris.get_payslip",
		map[string]any{"run_id": runID, "employee_id": empB})
	if rerr != nil {
		t.Fatalf("member PAT other's payslip: unexpected rpc error %+v", rerr)
	}
	if code := toolErrCode(t, res); code != "FORBIDDEN" {
		t.Fatalf("member PAT other's payslip: want FORBIDDEN, got %q", code)
	}
}

func TestSubmitPayrollPreviewMismatch(t *testing.T) {
	srv, _, ai, _, empA, _ := payrollFixture(t)
	runA := draftRun(t, srv, ai, "2026-10", "MY")
	runB := draftRun(t, srv, ai, "2026-11", "MY")
	out := previewRun(t, srv, ai, runA["id"].(string), payrollEmpArg(empA, 500000))
	previewID, _ := out["preview_id"].(string)

	// Preview belongs to run A; submitting run B must fail the gate.
	res := mustCall(t, srv, ai, "hris.submit_payroll", map[string]any{
		"run_id": runB["id"].(string), "preview_id": previewID,
		"version": runB["version"].(float64),
	})
	if code := toolErrCode(t, res); code != "PREVIEW_MISMATCH" {
		t.Errorf("code = %q, want PREVIEW_MISMATCH", code)
	}
}

func TestSubmitPayrollUnknownPreview(t *testing.T) {
	srv, _, ai, _, _, _ := payrollFixture(t)
	run := draftRun(t, srv, ai, "2026-10", "MY")
	res := mustCall(t, srv, ai, "hris.submit_payroll", map[string]any{
		"run_id": run["id"].(string), "preview_id": "pv-does-not-exist",
		"version": run["version"].(float64),
	})
	if code := toolErrCode(t, res); code != "PREVIEW_EXPIRED" {
		t.Errorf("code = %q, want PREVIEW_EXPIRED", code)
	}
}

func TestCancelPayrollRun(t *testing.T) {
	srv, st, ai, orgID, _, _ := payrollFixture(t)
	ctx := context.Background()
	run := draftRun(t, srv, ai, "2026-10", "MY")
	runID, _ := run["id"].(string)
	args := map[string]any{"run_id": runID, "version": run["version"].(float64)}

	res := mustCall(t, srv, ai, "hris.cancel_payroll_run", args)
	if res.IsError {
		t.Fatalf("want approval_required, got error: %s", res.Content[0].Text)
	}
	got := resultJSON(t, res)
	if got["approval_required"] != true || got["tier"] != "confirm" {
		t.Fatalf("want confirm approval_required, got %v", got)
	}
	token, _ := got["resume_token"].(string)
	res = mustCall(t, srv, ai, "hris.cancel_payroll_run",
		map[string]any{"resume_token": token, "approved": true})
	if res.IsError {
		t.Fatalf("cancel failed: %s", res.Content[0].Text)
	}
	final, err := st.GetPayrollRun(ctx, orgID, runID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.PayrollRunCancelled {
		t.Errorf("run status = %q, want cancelled", final.Status)
	}
	if !containsAction(agentAuditActions(t, st, orgID), "payroll_run.cancelled") {
		t.Error("missing payroll_run.cancelled audit event")
	}

	// A locked run cannot be cancelled.
	locked := draftRun(t, srv, ai, "2026-11", "MY")
	lr, err := st.GetPayrollRun(ctx, orgID, locked["id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	lr, err = st.TransitionPayrollRun(ctx, orgID, lr.ID, lr.Version, store.PayrollRunApproved, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionPayrollRun(ctx, orgID, lr.ID, lr.Version, store.PayrollRunLocked, "test"); err != nil {
		t.Fatal(err)
	}
	res = mustCall(t, srv, ai, "hris.cancel_payroll_run", map[string]any{
		"run_id": locked["id"].(string), "version": float64(3),
	})
	// The preview runs first (TierConfirm): it succeeds, then the handler
	// fails the illegal transition. Consume the token to reach the handler.
	got = resultJSON(t, res)
	token, _ = got["resume_token"].(string)
	res = mustCall(t, srv, ai, "hris.cancel_payroll_run",
		map[string]any{"resume_token": token, "approved": true})
	if code := toolErrCode(t, res); code != "ILLEGAL_RUN_TRANSITION" {
		t.Errorf("code = %q, want ILLEGAL_RUN_TRANSITION", code)
	}
}

func TestListPayrollRunsFilter(t *testing.T) {
	srv, _, ai, _, _, _ := payrollFixture(t)
	draftRun(t, srv, ai, "2026-10", "MY")
	draftRun(t, srv, ai, "2026-11", "ID")

	res := mustCall(t, srv, ai, "hris.list_payroll_runs",
		map[string]any{"period": "2026-11"})
	got := resultJSON(t, res)
	if data, _ := got["data"].([]any); len(data) != 1 {
		t.Fatalf("period filter returned %d runs, want 1", len(data))
	}

	res = mustCall(t, srv, ai, "hris.list_payroll_runs",
		map[string]any{"status": "bogus"})
	if code := toolErrCode(t, res); code != "VALIDATION_ERROR" {
		t.Errorf("code = %q, want VALIDATION_ERROR", code)
	}
}

func TestListPaySchedules(t *testing.T) {
	srv, _, ai, _, _, _ := payrollFixture(t)
	res := mustCall(t, srv, ai, "hris.list_pay_schedules", map[string]any{})
	got := resultJSON(t, res)
	cad, _ := got["payroll_cadence"].(map[string]any)
	if cad["cadence"] != "monthly" {
		t.Errorf("payroll_cadence = %v, want monthly", cad)
	}
}
