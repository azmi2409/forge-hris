package api

// Leave API tests over httptest: the RBAC matrix (member self-only,
// manager/admin gates), chain-aware approve/reject, the two-phase bulk
// approval (preview vs confirm, idempotent confirm), cancellation, and
// who-is-on-leave. The store underneath is the MemoryStore.
//
// leaveRoutes() is registered here explicitly because Worker C wires it
// into routes() separately: ensureLeaveRoutes tolerates the call having
// already happened (ServeMux panics on duplicate patterns).

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// ensureLeaveRoutes registers the leave routes unless routes() already did
// (Worker C's wiring). A duplicate ServeMux pattern panics; recover and
// carry on.
func ensureLeaveRoutes(t *testing.T, srv *Server) {
	t.Helper()
	defer func() {
		_ = recover()
	}()
	srv.leaveRoutes()
}

type leaveAPIFixture struct {
	srv         *Server
	st          store.Store
	ls          store.LeaveStore
	orgID       string
	adminID     string
	managerID   string
	memberID    string
	adminToken  string
	mgrToken    string
	memberToken string
	policyID    string
}

func newLeaveAPIFixture(t *testing.T) *leaveAPIFixture {
	t.Helper()
	srv, st := newTestServer(t)
	ensureLeaveRoutes(t, srv)
	ls, ok := st.(store.LeaveStore)
	if !ok {
		t.Fatal("store does not implement store.LeaveStore")
	}
	out := bootstrap(t, srv, "leave-api")
	ctx := context.Background()

	adminID := joinedEmployee(t, st, out.Organization.ID, "ADM-1", "admin@leave.test", store.RoleAdmin)
	managerID := joinedEmployee(t, st, out.Organization.ID, "MGR-1", "mgr@leave.test", store.RoleManager)
	// The member reports to the manager, so API-created requests default to
	// a [manager] approval chain.
	memberEmp, err := st.CreateEmployee(ctx, store.Employee{
		OrganizationID: out.Organization.ID,
		EmployeeNumber: "MBR-1",
		FirstName:      "Mem",
		LastName:       "Ber",
		Email:          "member@leave.test",
		Status:         store.InvitationJoined,
		ManagerID:      &managerID,
	})
	if err != nil {
		t.Fatalf("CreateEmployee member: %v", err)
	}
	if err := st.SetRole(ctx, out.Organization.ID, memberEmp.ID, store.RoleMember, "test"); err != nil {
		t.Fatalf("SetRole member: %v", err)
	}
	mintToken(t, st, out.Organization.ID, "pat", "admin", "leave-admin-token", &adminID, nil)
	mintToken(t, st, out.Organization.ID, "pat", "manager", "leave-mgr-token", &managerID, nil)
	mintToken(t, st, out.Organization.ID, "pat", "member", "leave-member-token", &memberEmp.ID, nil)

	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, id := range []string{adminID, managerID, memberEmp.ID} {
		emp, err := st.GetEmployee(ctx, out.Organization.ID, id)
		if err != nil {
			t.Fatalf("GetEmployee: %v", err)
		}
		emp.WorkStartDate = &start
		if _, err := st.UpdateEmployee(ctx, emp); err != nil {
			t.Fatalf("UpdateEmployee start date: %v", err)
		}
	}

	pol, err := ls.CreateTimeOffPolicy(ctx, store.TimeOffPolicy{
		OrganizationID: out.Organization.ID,
		Name:           "Annual",
		AccrualMode:    store.AccrualAnnually,
		AccrualAmount:  12,
	})
	if err != nil {
		t.Fatalf("CreateTimeOffPolicy: %v", err)
	}
	return &leaveAPIFixture{
		srv: srv, st: st, ls: ls, orgID: out.Organization.ID,
		adminID: adminID, managerID: managerID, memberID: memberEmp.ID,
		adminToken: "leave-admin-token", mgrToken: "leave-mgr-token",
		memberToken: "leave-member-token", policyID: pol.ID,
	}
}

// --- helpers ---

func leaveBase(f *leaveAPIFixture) string {
	return "/api/v1/organizations/" + f.orgID
}

func decodeBody(t *testing.T, rrBody []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(rrBody, v); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, rrBody)
	}
}

func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var ee errorEnvelope
	decodeBody(t, body, &ee)
	return ee.Code
}

func createLeaveRequest(t *testing.T, f *leaveAPIFixture, token string, body map[string]any) (int, []byte) {
	t.Helper()
	rr := doReq(t, f.srv, http.MethodPost, leaveBase(f)+"/leave-requests", token, body, nil)
	return rr.Code, rr.Body.Bytes()
}

// --- RBAC ---

func TestLeavePolicyRBAC(t *testing.T) {
	f := newLeaveAPIFixture(t)
	base := leaveBase(f)

	// Members cannot manage policies.
	rr := doReq(t, f.srv, http.MethodPost, base+"/leave-policies", f.memberToken, map[string]any{
		"name": "X", "accrual_mode": "monthly", "accrual_amount": 1,
	}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member create policy: want 403, got %d: %s", rr.Code, rr.Body.String())
	}
	// Managers can.
	rr = doReq(t, f.srv, http.MethodPost, base+"/leave-policies", f.mgrToken, map[string]any{
		"name": "Sick", "accrual_mode": "monthly", "accrual_amount": 1,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("manager create policy: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	// Members can read.
	rr = doReq(t, f.srv, http.MethodGet, base+"/leave-policies", f.memberToken, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("member list policies: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLeaveRequestSelfOnly(t *testing.T) {
	f := newLeaveAPIFixture(t)

	code, body := createLeaveRequest(t, f, f.memberToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-06",
	})
	if code != http.StatusCreated {
		t.Fatalf("member own request: want 201, got %d: %s", code, body)
	}
	var req store.TimeOffRequest
	decodeBody(t, body, &req)
	if req.Days != 5 {
		t.Fatalf("days = %v, want 5", req.Days)
	}
	// The chain defaulted to the member's manager.
	rr := doReq(t, f.srv, http.MethodGet, leaveBase(f)+"/leave-requests/"+req.ID+"/approvals", f.memberToken, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list approvals: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var page struct {
		Data []store.TimeOffApproval `json:"data"`
	}
	decodeBody(t, rr.Body.Bytes(), &page)
	if len(page.Data) != 1 || page.Data[0].ApproverEmployeeID != f.managerID {
		t.Fatalf("default chain wrong: %+v", page.Data)
	}

	// Members cannot request for someone else.
	code, body = createLeaveRequest(t, f, f.memberToken, map[string]any{
		"employee_id": f.managerID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-03",
	})
	if code != http.StatusForbidden {
		t.Fatalf("member request for other: want 403, got %d: %s", code, body)
	}
	// ... and cannot set an explicit approver chain.
	code, body = createLeaveRequest(t, f, f.memberToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-03",
		"approver_ids": []string{f.adminID},
	})
	if code != http.StatusForbidden {
		t.Fatalf("member explicit chain: want 403, got %d: %s", code, body)
	}
}

func TestLeaveRequestInsufficientBalanceAPI(t *testing.T) {
	f := newLeaveAPIFixture(t)
	mk := map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-06", // 5 of 12
	}
	if code, body := createLeaveRequest(t, f, f.memberToken, mk); code != http.StatusCreated {
		t.Fatalf("first request: want 201, got %d: %s", code, body)
	}
	mk["start_date"], mk["end_date"] = "2026-12-01", "2026-12-11" // 9 > 7 left
	code, body := createLeaveRequest(t, f, f.memberToken, mk)
	if code != http.StatusConflict || errorCode(t, body) != "INSUFFICIENT_BALANCE" {
		t.Fatalf("over-balance: want 409 INSUFFICIENT_BALANCE, got %d %s", code, body)
	}
}

// --- approval chains ---

type decisionOut struct {
	Request   store.TimeOffRequest    `json:"request"`
	Approvals []store.TimeOffApproval `json:"approvals"`
}

func decide(t *testing.T, f *leaveAPIFixture, token, id string, approved bool, version int64) (int, []byte) {
	t.Helper()
	action := "approve"
	if !approved {
		action = "reject"
	}
	rr := doReq(t, f.srv, http.MethodPost,
		leaveBase(f)+"/leave-requests/"+id+"/"+action, token,
		map[string]any{"version": version}, nil)
	return rr.Code, rr.Body.Bytes()
}

func TestApprovalChainAPI(t *testing.T) {
	f := newLeaveAPIFixture(t)

	// Chain [admin]: the manager holds the leave:approve scope but is
	// neither the current approver nor an admin -> 409.
	code, body := createLeaveRequest(t, f, f.mgrToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-03",
		"approver_ids": []string{f.adminID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %s", code, body)
	}
	var req store.TimeOffRequest
	decodeBody(t, body, &req)

	// Members lack the leave:approve scope -> 403.
	if code, body := decide(t, f, f.memberToken, req.ID, true, req.Version); code != http.StatusForbidden {
		t.Fatalf("member approve: want 403, got %d: %s", code, body)
	}
	// Manager has the scope but is not the current approver -> 409.
	code, body = decide(t, f, f.mgrToken, req.ID, true, req.Version)
	if code != http.StatusConflict || errorCode(t, body) != "NOT_CURRENT_APPROVER" {
		t.Fatalf("non-current approve: want 409 NOT_CURRENT_APPROVER, got %d %s", code, body)
	}
	// Admin (current) approves -> approved.
	code, body = decide(t, f, f.adminToken, req.ID, true, req.Version)
	if code != http.StatusOK {
		t.Fatalf("admin approve: want 200, got %d: %s", code, body)
	}
	var dec decisionOut
	decodeBody(t, body, &dec)
	if dec.Request.Status != store.TimeOffApproved {
		t.Fatalf("after admin approval: %+v", dec.Request)
	}

	// Chain [manager, admin]: order is enforced for non-admins.
	code, body = createLeaveRequest(t, f, f.mgrToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-09", "end_date": "2026-11-10",
		"approver_ids": []string{f.managerID, f.adminID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create 2: want 201, got %d: %s", code, body)
	}
	var req2 store.TimeOffRequest
	decodeBody(t, body, &req2)
	code, body = decide(t, f, f.mgrToken, req2.ID, true, req2.Version)
	if code != http.StatusOK {
		t.Fatalf("manager approve: want 200, got %d: %s", code, body)
	}
	decodeBody(t, body, &dec)
	if dec.Request.Status != store.TimeOffRequested || dec.Request.Version != req2.Version+1 {
		t.Fatalf("after first approval: %+v", dec.Request)
	}
	if dec.Approvals[0].Status != store.ApprovalApproved || dec.Approvals[1].Status != store.ApprovalPending {
		t.Fatalf("chain states wrong: %+v", dec.Approvals)
	}
	code, body = decide(t, f, f.adminToken, req2.ID, true, dec.Request.Version)
	if code != http.StatusOK {
		t.Fatalf("admin approve: want 200, got %d: %s", code, body)
	}
	decodeBody(t, body, &dec)
	if dec.Request.Status != store.TimeOffApproved {
		t.Fatalf("after final approval: %+v", dec.Request)
	}

	// Admin bypass: chain [manager], admin decides the manager's step.
	code, body = createLeaveRequest(t, f, f.mgrToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-16", "end_date": "2026-11-17",
		"approver_ids": []string{f.managerID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create 3: want 201, got %d: %s", code, body)
	}
	var req3 store.TimeOffRequest
	decodeBody(t, body, &req3)
	code, body = decide(t, f, f.adminToken, req3.ID, true, req3.Version)
	if code != http.StatusOK {
		t.Fatalf("admin bypass approve: want 200, got %d: %s", code, body)
	}
	decodeBody(t, body, &dec)
	if dec.Request.Status != store.TimeOffApproved ||
		dec.Approvals[0].Status != store.ApprovalApproved ||
		dec.Approvals[0].DecidedBy == nil || *dec.Approvals[0].DecidedBy != f.adminID {
		t.Fatalf("admin bypass result wrong: %+v", dec)
	}
}

func TestRejectShortCircuitsAPI(t *testing.T) {
	f := newLeaveAPIFixture(t)
	code, body := createLeaveRequest(t, f, f.mgrToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-03",
		"approver_ids": []string{f.managerID, f.adminID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var req store.TimeOffRequest
	decodeBody(t, body, &req)

	code, body = decide(t, f, f.mgrToken, req.ID, false, req.Version)
	if code != http.StatusOK {
		t.Fatalf("reject: want 200, got %d: %s", code, body)
	}
	var dec decisionOut
	decodeBody(t, body, &dec)
	if dec.Request.Status != store.TimeOffRejected {
		t.Fatalf("after reject: %+v", dec.Request)
	}
	code, body = decide(t, f, f.adminToken, req.ID, true, dec.Request.Version)
	if code != http.StatusConflict || errorCode(t, body) != "INVALID_TRANSITION" {
		t.Fatalf("decide rejected: want 409 INVALID_TRANSITION, got %d %s", code, body)
	}
}

func TestAdminApprovesChainlessRequestAPI(t *testing.T) {
	f := newLeaveAPIFixture(t)
	// The admin has no manager: chainless request.
	code, body := createLeaveRequest(t, f, f.adminToken, map[string]any{
		"employee_id": f.adminID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-03",
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var req store.TimeOffRequest
	decodeBody(t, body, &req)
	code, body = decide(t, f, f.adminToken, req.ID, true, req.Version)
	if code != http.StatusOK {
		t.Fatalf("admin approve chainless: want 200, got %d: %s", code, body)
	}
	var dec decisionOut
	decodeBody(t, body, &dec)
	if dec.Request.Status != store.TimeOffApproved {
		t.Fatalf("chainless after admin approve: %+v", dec.Request)
	}
}

// --- bulk approval ---

type bulkPreviewOut struct {
	Results []struct {
		RequestID       string  `json:"request_id"`
		CanApprove      bool    `json:"can_approve"`
		Reason          string  `json:"reason"`
		CurrentApprover *string `json:"current_approver"`
		Status          string  `json:"status"`
	} `json:"results"`
}

type bulkConfirmOut struct {
	Approved []store.TimeOffRequest `json:"approved"`
}

func TestBulkApprovePreviewVsConfirm(t *testing.T) {
	f := newLeaveAPIFixture(t)
	base := leaveBase(f)

	var ids []string
	for _, dates := range [][2]string{{"2026-11-02", "2026-11-03"}, {"2026-11-09", "2026-11-10"}} {
		code, body := createLeaveRequest(t, f, f.memberToken, map[string]any{
			"employee_id": f.memberID, "policy_id": f.policyID,
			"start_date": dates[0], "end_date": dates[1],
		})
		if code != http.StatusCreated {
			t.Fatalf("create: %d %s", code, body)
		}
		var req store.TimeOffRequest
		decodeBody(t, body, &req)
		ids = append(ids, req.ID)
	}

	// Preview as admin: no mutation.
	rr := doReq(t, f.srv, http.MethodPost, base+"/leave-requests/bulk-approve", f.adminToken,
		map[string]any{"ids": ids, "preview": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var pv bulkPreviewOut
	decodeBody(t, rr.Body.Bytes(), &pv)
	if len(pv.Results) != 2 || !pv.Results[0].CanApprove || !pv.Results[1].CanApprove {
		t.Fatalf("preview results: %+v", pv.Results)
	}
	for _, id := range ids {
		grr := doReq(t, f.srv, http.MethodGet, base+"/leave-requests/"+id, f.adminToken, nil, nil)
		var req store.TimeOffRequest
		decodeBody(t, grr.Body.Bytes(), &req)
		if req.Status != store.TimeOffRequested {
			t.Fatalf("preview mutated request %s: %s", id, req.Status)
		}
	}

	// Confirm with an idempotency key.
	hdr := map[string]string{"Idempotency-Key": "bulk-1"}
	rr = doReq(t, f.srv, http.MethodPost, base+"/leave-requests/bulk-approve", f.adminToken,
		map[string]any{"ids": ids, "preview": false}, hdr)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	firstBody := rr.Body.String()
	var cf bulkConfirmOut
	decodeBody(t, rr.Body.Bytes(), &cf)
	if len(cf.Approved) != 2 || cf.Approved[0].Status != store.TimeOffApproved {
		t.Fatalf("confirm result: %+v", cf.Approved)
	}

	// Replaying the same key returns the identical stored response without
	// re-executing (the requests stay approved at the same version).
	rr = doReq(t, f.srv, http.MethodPost, base+"/leave-requests/bulk-approve", f.adminToken,
		map[string]any{"ids": ids, "preview": false}, hdr)
	if rr.Code != http.StatusOK || rr.Body.String() != firstBody {
		t.Fatalf("idempotent replay: want identical 200, got %d", rr.Code)
	}

	// A confirm mixing an already-approved request fails all-or-nothing.
	rr = doReq(t, f.srv, http.MethodPost, base+"/leave-requests/bulk-approve", f.adminToken,
		map[string]any{"ids": ids, "preview": false},
		map[string]string{"Idempotency-Key": "bulk-2"})
	if rr.Code != http.StatusConflict || errorCode(t, rr.Body.Bytes()) != "BULK_APPROVAL_FAILED" {
		t.Fatalf("bulk on decided: want 409 BULK_APPROVAL_FAILED, got %d %s", rr.Code, rr.Body.String())
	}
}

func TestBulkApprovePreviewGate(t *testing.T) {
	f := newLeaveAPIFixture(t)
	base := leaveBase(f)

	// Chain [admin]: the manager is not the current approver.
	code, body := createLeaveRequest(t, f, f.mgrToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-03",
		"approver_ids": []string{f.adminID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var req store.TimeOffRequest
	decodeBody(t, body, &req)

	rr := doReq(t, f.srv, http.MethodPost, base+"/leave-requests/bulk-approve", f.mgrToken,
		map[string]any{"ids": []string{req.ID}, "preview": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	var pv bulkPreviewOut
	decodeBody(t, rr.Body.Bytes(), &pv)
	if pv.Results[0].CanApprove || pv.Results[0].Reason == "" {
		t.Fatalf("manager preview on admin's step: %+v", pv.Results)
	}
}

// --- cancel ---

func TestCancelLeaveRequestAPI(t *testing.T) {
	f := newLeaveAPIFixture(t)
	base := leaveBase(f)

	code, body := createLeaveRequest(t, f, f.memberToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-11-02", "end_date": "2026-11-03",
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var req store.TimeOffRequest
	decodeBody(t, body, &req)

	rr := doReq(t, f.srv, http.MethodPost, base+"/leave-requests/"+req.ID+"/cancel", f.memberToken,
		map[string]any{"version": req.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("member cancel own: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var cancelled store.TimeOffRequest
	decodeBody(t, rr.Body.Bytes(), &cancelled)
	if cancelled.Status != store.TimeOffCancelled {
		t.Fatalf("cancelled status: %s", cancelled.Status)
	}

	// Cancelling again is an invalid transition.
	rr = doReq(t, f.srv, http.MethodPost, base+"/leave-requests/"+req.ID+"/cancel", f.memberToken,
		map[string]any{"version": cancelled.Version}, nil)
	if rr.Code != http.StatusConflict || errorCode(t, rr.Body.Bytes()) != "INVALID_TRANSITION" {
		t.Fatalf("double cancel: want 409 INVALID_TRANSITION, got %d %s", rr.Code, rr.Body.String())
	}

	// A member cannot cancel someone else's request.
	code, body = createLeaveRequest(t, f, f.mgrToken, map[string]any{
		"employee_id": f.managerID, "policy_id": f.policyID,
		"start_date": "2026-11-09", "end_date": "2026-11-10",
	})
	if code != http.StatusCreated {
		t.Fatalf("create mgr request: %d %s", code, body)
	}
	var other store.TimeOffRequest
	decodeBody(t, body, &other)
	rr = doReq(t, f.srv, http.MethodPost, base+"/leave-requests/"+other.ID+"/cancel", f.memberToken,
		map[string]any{"version": other.Version}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member cancel other's: want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

// --- balances, calendars, who-is-on-leave ---

func TestRecalculateBalanceAPI(t *testing.T) {
	f := newLeaveAPIFixture(t)
	base := leaveBase(f)

	rr := doReq(t, f.srv, http.MethodPost, base+"/leave-balances/recalculate", f.memberToken,
		map[string]any{"employee_id": f.memberID, "policy_id": f.policyID}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("recalculate own: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var bal store.LeaveBalance
	decodeBody(t, rr.Body.Bytes(), &bal)
	if bal.Period != "2026" || bal.Accrued != 12 || bal.Balance != 12 {
		t.Fatalf("recalculated balance: %+v", bal)
	}

	// Members cannot recalculate someone else's balance.
	rr = doReq(t, f.srv, http.MethodPost, base+"/leave-balances/recalculate", f.memberToken,
		map[string]any{"employee_id": f.managerID, "policy_id": f.policyID}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("recalculate other's: want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestHolidayCalendarAPI(t *testing.T) {
	f := newLeaveAPIFixture(t)
	base := leaveBase(f)

	rr := doReq(t, f.srv, http.MethodPost, base+"/holiday-calendars", f.memberToken, map[string]any{
		"name": "X", "country_code": "MY",
	}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member create calendar: want 403, got %d", rr.Code)
	}
	rr = doReq(t, f.srv, http.MethodPost, base+"/holiday-calendars", f.mgrToken, map[string]any{
		"name": "MY Holidays", "country_code": "MY",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("manager create calendar: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var cal store.HolidayCalendar
	decodeBody(t, rr.Body.Bytes(), &cal)

	hpath := base + "/holiday-calendars/" + cal.ID + "/holidays"
	rr = doReq(t, f.srv, http.MethodPost, hpath, f.mgrToken, map[string]any{
		"date": "2026-01-01", "name": "New Year",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("add holiday: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodPost, hpath, f.mgrToken, map[string]any{
		"date": "2026-01-01", "name": "Dupe",
	}, nil)
	if rr.Code != http.StatusConflict || errorCode(t, rr.Body.Bytes()) != "ALREADY_EXISTS" {
		t.Fatalf("duplicate holiday: want 409 ALREADY_EXISTS, got %d %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, f.srv, http.MethodGet, hpath, f.memberToken, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("member list holidays: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWhoIsOnLeaveAPI(t *testing.T) {
	f := newLeaveAPIFixture(t)
	base := leaveBase(f)

	code, body := createLeaveRequest(t, f, f.memberToken, map[string]any{
		"employee_id": f.memberID, "policy_id": f.policyID,
		"start_date": "2026-10-15", "end_date": "2026-10-17",
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var req store.TimeOffRequest
	decodeBody(t, body, &req)
	// Admin approves the manager's pending step (admin bypass).
	if code, body := decide(t, f, f.adminToken, req.ID, true, req.Version); code != http.StatusOK {
		t.Fatalf("admin approve: %d %s", code, body)
	}

	rr := doReq(t, f.srv, http.MethodGet, base+"/who-is-on-leave?date=2026-10-16", f.memberToken, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("who-is-on-leave: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Date    string `json:"date"`
		OnLeave []struct {
			RequestID    string `json:"request_id"`
			EmployeeID   string `json:"employee_id"`
			EmployeeName string `json:"employee_name"`
			PolicyName   string `json:"policy_name"`
			IsUnpaid     bool   `json:"is_unpaid"`
		} `json:"on_leave"`
	}
	decodeBody(t, rr.Body.Bytes(), &out)
	if out.Date != "2026-10-16" || len(out.OnLeave) != 1 {
		t.Fatalf("who-is-on-leave: %+v", out)
	}
	e := out.OnLeave[0]
	if e.RequestID != req.ID || e.EmployeeID != f.memberID ||
		e.EmployeeName == "" || e.PolicyName != "Annual" || e.IsUnpaid {
		t.Fatalf("entry wrong: %+v", e)
	}

	rr = doReq(t, f.srv, http.MethodGet, base+"/who-is-on-leave?date=2026-10-19", f.memberToken, nil, nil)
	var out2 struct {
		OnLeave []any `json:"on_leave"`
	}
	decodeBody(t, rr.Body.Bytes(), &out2)
	if len(out2.OnLeave) != 0 {
		t.Fatalf("nobody should be on leave: %+v", out2)
	}
}
