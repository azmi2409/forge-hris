package mcp

// MCP server tests over the MemoryStore: auth, approval gates (confirm and
// never tiers), scope/role gates, happy-path e2e, and the JSON-RPC/HTTP
// transports. Follows the repo's test style (fresh store per test, direct
// store setup like api/api_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

var mcpTestCounter atomic.Int64

func mcpUniq(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, mcpTestCounter.Add(1))
}

func newTestMCP(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st := store.NewMemoryStore()
	return NewServer(st, nil), st
}

func testOrg(t *testing.T, st store.Store) string {
	t.Helper()
	org, err := st.CreateOrganization(context.Background(), store.Organization{
		Name: "Test Co", Slug: mcpUniq("test-co"), CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org.ID
}

// joinedEmployee creates a joined employee with the given role, mirroring
// the api test helper.
func joinedEmployee(t *testing.T, st store.Store, orgID, number, email string, role store.Role) string {
	t.Helper()
	ctx := context.Background()
	emp, err := st.CreateEmployee(ctx, store.Employee{
		OrganizationID: orgID,
		EmployeeNumber: number,
		FirstName:      "Test",
		LastName:       string(role),
		Email:          email,
		Status:         store.InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	if err := st.SetRole(ctx, orgID, emp.ID, role, "test"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	return emp.ID
}

// mintToken issues a credential via the store; the plaintext is what the
// MCP server accepts as a bearer token.
func mintToken(t *testing.T, st store.Store, orgID, kind, name, plaintext string, employeeID *string, scopes []string) {
	t.Helper()
	_, err := st.IssueCredential(context.Background(), store.Credential{
		OrganizationID: orgID,
		Kind:           store.CredentialKind(kind),
		Name:           name,
		EmployeeID:     employeeID,
		Scopes:         scopes,
	}, plaintext)
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
}

func mustAuth(t *testing.T, srv *Server, token string) *AuthInfo {
	t.Helper()
	ai, rerr := srv.Authenticate(context.Background(), token)
	if rerr != nil {
		t.Fatalf("Authenticate: %v", rerr)
	}
	return ai
}

// mustCall executes a tool and fails the test on request-level errors.
func mustCall(t *testing.T, srv *Server, ai *AuthInfo, name string, args map[string]any) *toolCallResult {
	t.Helper()
	res, rerr := srv.executeTool(context.Background(), ai, name, args)
	if rerr != nil {
		t.Fatalf("executeTool %s: rpc error %d: %s", name, rerr.Code, rerr.Message)
	}
	return res
}

// resultJSON parses the text content of a tool result.
func resultJSON(t *testing.T, res *toolCallResult) map[string]any {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(res.Content))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("result text is not JSON: %v\n%s", err, res.Content[0].Text)
	}
	return out
}

func toolErrCode(t *testing.T, res *toolCallResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("want isError=true, got success: %s", res.Content[0].Text)
	}
	return resultJSON(t, res)["code"].(string)
}

func employeeCount(t *testing.T, st store.Store, orgID string) int {
	t.Helper()
	emps, _, err := st.ListEmployees(context.Background(), orgID, store.EmployeeFilter{}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatalf("ListEmployees: %v", err)
	}
	return len(emps)
}

func agentAuditActions(t *testing.T, st store.Store, orgID string) []string {
	t.Helper()
	events, _, err := st.ListAuditEvents(context.Background(), orgID,
		store.AuditFilter{ActorType: store.ActorAgent}, store.Page{Limit: 1000})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	actions := make([]string, 0, len(events))
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	return actions
}

func containsAction(actions []string, want string) bool {
	for _, a := range actions {
		if a == want {
			return true
		}
	}
	return false
}

// --- auth ---

func TestAuthenticate(t *testing.T) {
	srv, st := newTestMCP(t)
	orgID := testOrg(t, st)
	empID := joinedEmployee(t, st, orgID, "E-1", mcpUniq("admin")+"@t.test", store.RoleAdmin)
	mintToken(t, st, orgID, "pat", "admin", "tok-admin", &empID, nil)

	t.Run("unknown token rejected", func(t *testing.T) {
		_, rerr := srv.Authenticate(context.Background(), "hris_nope")
		if rerr == nil || rerr.Code != rpcUnauthenticated {
			t.Fatalf("want unauthenticated, got %+v", rerr)
		}
	})

	t.Run("revoked token rejected", func(t *testing.T) {
		ctx := context.Background()
		mintToken(t, st, orgID, "pat", "doomed", "tok-doomed", &empID, nil)
		cred, err := st.GetCredential(ctx, orgID, mustCredID(t, st, orgID, "tok-doomed"))
		if err != nil {
			t.Fatalf("GetCredential: %v", err)
		}
		if err := st.RevokeCredential(ctx, orgID, cred.ID); err != nil {
			t.Fatalf("RevokeCredential: %v", err)
		}
		if _, rerr := srv.Authenticate(ctx, "tok-doomed"); rerr == nil || rerr.Code != rpcUnauthenticated {
			t.Fatalf("want unauthenticated for revoked token, got %+v", rerr)
		}
	})

	t.Run("PAT inherits role scopes", func(t *testing.T) {
		ai := mustAuth(t, srv, "tok-admin")
		if ai.OrgID != orgID {
			t.Errorf("OrgID = %q, want %q", ai.OrgID, orgID)
		}
		if ai.Kind != store.CredentialPAT {
			t.Errorf("Kind = %q, want pat", ai.Kind)
		}
		if !hasScope(store.ScopeLeaveApprove, ai) {
			t.Error("admin PAT should hold leave:approve")
		}
	})

	t.Run("service account carries explicit scopes", func(t *testing.T) {
		mintToken(t, st, orgID, "service_account", "sa", "tok-sa", nil, []string{store.ScopeTimeRead})
		ai := mustAuth(t, srv, "tok-sa")
		if ai.EmployeeID != nil {
			t.Error("service account must not have an employee")
		}
		if !hasScope(store.ScopeTimeRead, ai) || hasScope(store.ScopeTimeWrite, ai) {
			t.Errorf("unexpected scopes: %v", ai.Scopes)
		}
		// time:write tool must be forbidden at the JSON-RPC level.
		_, rerr := srv.executeTool(context.Background(), ai, "hris.clock_in", map[string]any{})
		if rerr == nil || rerr.Code != rpcForbidden {
			t.Fatalf("want forbidden, got %+v", rerr)
		}
	})
}

func mustCredID(t *testing.T, st store.Store, orgID, plaintext string) string {
	t.Helper()
	cred, err := st.GetCredentialByTokenHash(context.Background(), hashToken(plaintext))
	if err != nil {
		t.Fatalf("GetCredentialByTokenHash: %v", err)
	}
	return cred.ID
}

// --- approval gates: confirm tier ---

func adminFixture(t *testing.T) (*Server, store.Store, *AuthInfo, string) {
	t.Helper()
	srv, st := newTestMCP(t)
	orgID := testOrg(t, st)
	empID := joinedEmployee(t, st, orgID, "E-1", mcpUniq("admin")+"@t.test", store.RoleAdmin)
	mintToken(t, st, orgID, "pat", "admin", "tok-admin", &empID, nil)
	return srv, st, mustAuth(t, srv, "tok-admin"), orgID
}

func TestConfirmApprovalGate(t *testing.T) {
	srv, st, ai, orgID := adminFixture(t)
	before := employeeCount(t, st, orgID)
	args := map[string]any{
		"first_name": "Ada", "last_name": "Lovelace", "email": mcpUniq("ada") + "@t.test",
	}

	t.Run("first call returns approval_required and executes nothing", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.create_employee", args)
		if res.IsError {
			t.Fatalf("want approval_required result, got error: %s", res.Content[0].Text)
		}
		out := resultJSON(t, res)
		if out["approval_required"] != true {
			t.Fatalf("approval_required != true: %v", out)
		}
		token, _ := out["resume_token"].(string)
		if token == "" {
			t.Fatal("resume_token is empty")
		}
		if out["tier"] != "confirm" {
			t.Errorf("tier = %v, want confirm", out["tier"])
		}
		if out["preview"] == nil {
			t.Error("preview is missing")
		}
		if got := employeeCount(t, st, orgID); got != before {
			t.Errorf("store changed: %d employees, want %d", got, before)
		}
		if containsAction(agentAuditActions(t, st, orgID), "employee.created") {
			t.Error("audit event written before approval")
		}
	})

	resumeOf := func(t *testing.T) string {
		t.Helper()
		// Re-issue: the helper env var is per-test, so re-run the first
		// call to get a fresh token for independent subtests.
		res := mustCall(t, srv, ai, "hris.create_employee", args)
		return resultJSON(t, res)["resume_token"].(string)
	}

	t.Run("resume without approved:true is rejected", func(t *testing.T) {
		token := resumeOf(t)
		res := mustCall(t, srv, ai, "hris.create_employee", map[string]any{"resume_token": token})
		if got := toolErrCode(t, res); got != "APPROVAL_NOT_CONFIRMED" {
			t.Errorf("code = %q, want APPROVAL_NOT_CONFIRMED", got)
		}
		if got := employeeCount(t, st, orgID); got != before {
			t.Errorf("store changed without approval: %d employees", got)
		}
	})

	t.Run("unknown resume token is rejected", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.create_employee",
			map[string]any{"resume_token": "nope", "approved": true})
		if got := toolErrCode(t, res); got != "APPROVAL_NOT_FOUND" {
			t.Errorf("code = %q, want APPROVAL_NOT_FOUND", got)
		}
	})

	t.Run("resume with approved:true executes and audits as agent", func(t *testing.T) {
		token := resumeOf(t)
		res := mustCall(t, srv, ai, "hris.create_employee",
			map[string]any{"resume_token": token, "approved": true})
		if res.IsError {
			t.Fatalf("resume failed: %s", res.Content[0].Text)
		}
		out := resultJSON(t, res)
		emp, _ := out["employee"].(map[string]any)
		if emp["first_name"] != "Ada" {
			t.Errorf("created wrong employee: %v", emp)
		}
		if got := employeeCount(t, st, orgID); got != before+1 {
			t.Errorf("employee count = %d, want %d", got, before+1)
		}
		actions := agentAuditActions(t, st, orgID)
		if !containsAction(actions, "employee.created") {
			t.Errorf("no employee.created agent audit event; actions=%v", actions)
		}
		// The token is single-use.
		res2 := mustCall(t, srv, ai, "hris.create_employee",
			map[string]any{"resume_token": token, "approved": true})
		if got := toolErrCode(t, res2); got != "APPROVAL_NOT_FOUND" {
			t.Errorf("reused token: code = %q, want APPROVAL_NOT_FOUND", got)
		}
	})

	t.Run("expired resume token is rejected", func(t *testing.T) {
		token := resumeOf(t)
		srv.approvals.mu.Lock()
		if pa, ok := srv.approvals.pending[token]; ok {
			pa.createdAt = time.Now().Add(-11 * time.Minute)
		} else {
			srv.approvals.mu.Unlock()
			t.Fatal("pending approval not found")
		}
		srv.approvals.mu.Unlock()
		res := mustCall(t, srv, ai, "hris.create_employee",
			map[string]any{"resume_token": token, "approved": true})
		if got := toolErrCode(t, res); got != "APPROVAL_EXPIRED" {
			t.Errorf("code = %q, want APPROVAL_EXPIRED", got)
		}
	})
}

// --- approval gates: never tier (two-phase bulk) ---

func bulkFixture(t *testing.T) (*Server, store.Store, *AuthInfo, string, []string) {
	t.Helper()
	srv, st := newTestMCP(t)
	ctx := context.Background()
	orgID := testOrg(t, st)
	mgrID := joinedEmployee(t, st, orgID, "E-mgr", mcpUniq("mgr")+"@t.test", store.RoleManager)
	memID := joinedEmployee(t, st, orgID, "E-mem", mcpUniq("mem")+"@t.test", store.RoleMember)
	mintToken(t, st, orgID, "pat", "mgr", "tok-mgr", &mgrID, nil)
	pol, err := st.CreateTimeOffPolicy(ctx, store.TimeOffPolicy{
		OrganizationID: orgID, Name: "Annual", AccrualMode: store.AccrualAnnually, AccrualAmount: 12,
	})
	if err != nil {
		t.Fatalf("CreateTimeOffPolicy: %v", err)
	}
	var ids []string
	for i := 0; i < 2; i++ {
		lr, err := st.CreateTimeOffRequest(ctx, store.TimeOffRequest{
			OrganizationID: orgID, EmployeeID: memID, PolicyID: pol.ID,
			StartDate: "2027-04-0" + fmt.Sprint(i+6), EndDate: "2027-04-0" + fmt.Sprint(i+6),
			IsUnpaid: true, ApproverIDs: []string{mgrID},
		}, nil)
		if err != nil {
			t.Fatalf("CreateTimeOffRequest: %v", err)
		}
		ids = append(ids, lr.ID)
	}
	return srv, st, mustAuth(t, srv, "tok-mgr"), orgID, ids
}

func TestBulkNeverAuto(t *testing.T) {
	srv, st, ai, orgID, ids := bulkFixture(t)

	t.Run("first call returns preview with counts, applies nothing", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.approve_leave_bulk", map[string]any{"request_ids": ids})
		if res.IsError {
			t.Fatalf("want preview, got error: %s", res.Content[0].Text)
		}
		out := resultJSON(t, res)
		if out["approval_required"] != true || out["tier"] != "never" {
			t.Fatalf("want never-tier approval_required, got %v", out)
		}
		preview, _ := out["preview"].(map[string]any)
		counts, _ := preview["counts"].(map[string]any)
		if counts["total"] != float64(2) || counts["approvable"] != float64(2) {
			t.Errorf("counts = %v, want total:2 approvable:2", counts)
		}
		for _, id := range ids {
			lr, err := st.GetTimeOffRequest(context.Background(), orgID, id)
			if err != nil {
				t.Fatal(err)
			}
			if lr.Status != store.TimeOffRequested {
				t.Errorf("request %s changed to %s before approval", id, lr.Status)
			}
		}
	})

	t.Run("resume without confirmed:true is rejected", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.approve_leave_bulk", map[string]any{"request_ids": ids})
		token := resultJSON(t, res)["resume_token"].(string)
		res2 := mustCall(t, srv, ai, "hris.approve_leave_bulk",
			map[string]any{"resume_token": token, "approved": true})
		if got := toolErrCode(t, res2); got != "APPROVAL_NOT_CONFIRMED" {
			t.Errorf("code = %q, want APPROVAL_NOT_CONFIRMED", got)
		}
		lr, _ := st.GetTimeOffRequest(context.Background(), orgID, ids[0])
		if lr.Status != store.TimeOffRequested {
			t.Errorf("request changed without confirmed:true: %s", lr.Status)
		}
	})

	t.Run("resume with confirmed:true executes all-or-nothing", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.approve_leave_bulk", map[string]any{"request_ids": ids})
		token := resultJSON(t, res)["resume_token"].(string)
		res2 := mustCall(t, srv, ai, "hris.approve_leave_bulk",
			map[string]any{"resume_token": token, "confirmed": true})
		if res2.IsError {
			t.Fatalf("bulk approve failed: %s", res2.Content[0].Text)
		}
		for _, id := range ids {
			lr, err := st.GetTimeOffRequest(context.Background(), orgID, id)
			if err != nil {
				t.Fatal(err)
			}
			if lr.Status != store.TimeOffApproved {
				t.Errorf("request %s = %s, want approved", id, lr.Status)
			}
		}
		if !containsAction(agentAuditActions(t, st, orgID), "leave_request.bulk_approved") {
			t.Error("no leave_request.bulk_approved agent audit event")
		}
	})
}

// --- happy-path e2e ---

func TestE2EClockAndLeave(t *testing.T) {
	srv, st := newTestMCP(t)
	ctx := context.Background()
	orgID := testOrg(t, st)
	memID := joinedEmployee(t, st, orgID, "E-1", mcpUniq("mem")+"@t.test", store.RoleMember)
	mintToken(t, st, orgID, "pat", "mem", "tok-mem", &memID, nil)
	ai := mustAuth(t, srv, "tok-mem")
	now := time.Now().UTC().Format(time.RFC3339)

	t.Run("clock_in opens an entry", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.clock_in", map[string]any{"occurred_at": now})
		if res.IsError {
			t.Fatalf("clock_in: %s", res.Content[0].Text)
		}
		entry := resultJSON(t, res)["time_entry"].(map[string]any)
		if entry["kind"] != "in" {
			t.Errorf("kind = %v, want in", entry["kind"])
		}
		_, open, err := st.OpenTimeEntry(ctx, orgID, memID)
		if err != nil || !open {
			t.Errorf("OpenTimeEntry: open=%v err=%v", open, err)
		}
	})

	t.Run("double clock_in is rejected", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.clock_in", map[string]any{"occurred_at": now})
		if got := toolErrCode(t, res); got != "ALREADY_CLOCKED_IN" {
			t.Errorf("code = %q, want ALREADY_CLOCKED_IN", got)
		}
	})

	t.Run("clock_out with idempotency key replays", func(t *testing.T) {
		res := mustCall(t, srv, ai, "hris.clock_out",
			map[string]any{"occurred_at": now, "idempotency_key": "e2e-1"})
		if res.IsError {
			t.Fatalf("clock_out: %s", res.Content[0].Text)
		}
		firstID := resultJSON(t, res)["time_entry"].(map[string]any)["id"]
		res2 := mustCall(t, srv, ai, "hris.clock_out",
			map[string]any{"occurred_at": now, "idempotency_key": "e2e-1"})
		if res2.IsError {
			t.Fatalf("replay clock_out: %s", res2.Content[0].Text)
		}
		if secondID := resultJSON(t, res2)["time_entry"].(map[string]any)["id"]; secondID != firstID {
			t.Errorf("replay returned different entry %v, want %v", secondID, firstID)
		}
	})

	t.Run("request_leave creates a request", func(t *testing.T) {
		pol, err := st.CreateTimeOffPolicy(ctx, store.TimeOffPolicy{
			OrganizationID: orgID, Name: "Annual", AccrualMode: store.AccrualAnnually, AccrualAmount: 12,
		})
		if err != nil {
			t.Fatalf("CreateTimeOffPolicy: %v", err)
		}
		res := mustCall(t, srv, ai, "hris.request_leave", map[string]any{
			"employee_id": memID, "policy_id": pol.ID,
			"start_date": "2027-05-04", "end_date": "2027-05-05", "is_unpaid": true,
		})
		if res.IsError {
			t.Fatalf("request_leave: %s", res.Content[0].Text)
		}
		req := resultJSON(t, res)["request"].(map[string]any)
		if req["status"] != "requested" {
			t.Errorf("status = %v, want requested", req["status"])
		}
	})

	t.Run("audit shows agent actor", func(t *testing.T) {
		actions := agentAuditActions(t, st, orgID)
		for _, want := range []string{"time_entry.clock_in", "time_entry.clock_out", "leave_request.created"} {
			if !containsAction(actions, want) {
				t.Errorf("missing agent audit action %q; got %v", want, actions)
			}
		}
		events, _, err := st.ListAuditEvents(ctx, orgID,
			store.AuditFilter{ActorType: store.ActorAgent, Action: "time_entry.clock_in"}, store.Page{Limit: 10})
		if err != nil || len(events) == 0 {
			t.Fatalf("no agent clock_in events: %v", err)
		}
		if events[0].ActorID == "" {
			t.Error("agent audit event has no actor id")
		}
		if events[0].OnBehalfOf != nil {
			t.Errorf("OnBehalfOf should be nil without delegation, got %v", *events[0].OnBehalfOf)
		}
	})
}

func TestDelegatedActAs(t *testing.T) {
	srv, st := newTestMCP(t)
	ctx := context.Background()
	orgID := testOrg(t, st)
	memID := joinedEmployee(t, st, orgID, "E-1", mcpUniq("mem")+"@t.test", store.RoleMember)
	mintToken(t, st, orgID, "pat", "mem", "tok-mem", &memID, nil)
	ai := mustAuth(t, srv, "tok-mem")

	res := mustCall(t, srv, ai, "hris.clock_in", map[string]any{
		"occurred_at":  time.Now().UTC().Format(time.RFC3339),
		"on_behalf_of": "hr-agent-7",
	})
	if res.IsError {
		t.Fatalf("clock_in: %s", res.Content[0].Text)
	}
	events, _, err := st.ListAuditEvents(ctx, orgID,
		store.AuditFilter{ActorType: store.ActorAgent, Action: "time_entry.clock_in"}, store.Page{Limit: 10})
	if err != nil || len(events) == 0 {
		t.Fatalf("no agent events: %v", err)
	}
	if events[0].OnBehalfOf == nil || *events[0].OnBehalfOf != "hr-agent-7" {
		t.Errorf("OnBehalfOf not recorded: %+v", events[0].OnBehalfOf)
	}
}

// --- scope/role gates ---

func TestGates(t *testing.T) {
	srv, st := newTestMCP(t)
	ctx := context.Background()
	orgID := testOrg(t, st)
	adminID := joinedEmployee(t, st, orgID, "E-1", mcpUniq("admin")+"@t.test", store.RoleAdmin)
	memID := joinedEmployee(t, st, orgID, "E-2", mcpUniq("mem")+"@t.test", store.RoleMember)
	otherID := joinedEmployee(t, st, orgID, "E-3", mcpUniq("other")+"@t.test", store.RoleMember)
	mintToken(t, st, orgID, "pat", "admin", "tok-admin", &adminID, nil)
	mintToken(t, st, orgID, "pat", "mem", "tok-mem", &memID, nil)
	adminAI := mustAuth(t, srv, "tok-admin")
	memAI := mustAuth(t, srv, "tok-mem")

	t.Run("member cannot use admin tool", func(t *testing.T) {
		_, rerr := srv.executeTool(ctx, memAI, "hris.deactivate_employee",
			map[string]any{"employee_id": otherID, "version": float64(1)})
		if rerr == nil || rerr.Code != rpcForbidden {
			t.Fatalf("want forbidden, got %+v", rerr)
		}
	})

	t.Run("member cannot read another employee", func(t *testing.T) {
		res := mustCall(t, srv, memAI, "hris.get_employee", map[string]any{"employee_id": otherID})
		if got := toolErrCode(t, res); got != "FORBIDDEN" {
			t.Errorf("code = %q, want FORBIDDEN", got)
		}
	})

	t.Run("member cannot clock in for another employee", func(t *testing.T) {
		res := mustCall(t, srv, memAI, "hris.clock_in", map[string]any{"employee_id": otherID})
		if got := toolErrCode(t, res); got != "FORBIDDEN" {
			t.Errorf("code = %q, want FORBIDDEN", got)
		}
	})

	t.Run("manager can act for another employee", func(t *testing.T) {
		res := mustCall(t, srv, adminAI, "hris.clock_in",
			map[string]any{"employee_id": memID, "occurred_at": time.Now().UTC().Format(time.RFC3339)})
		if res.IsError {
			t.Fatalf("admin clock_in for member: %s", res.Content[0].Text)
		}
	})

	t.Run("approve_leave without scope is forbidden", func(t *testing.T) {
		// memAI holds leave:read/write but not leave:approve.
		_, rerr := srv.executeTool(ctx, memAI, "hris.approve_leave",
			map[string]any{"request_id": "x", "version": float64(1)})
		if rerr == nil || rerr.Code != rpcForbidden {
			t.Fatalf("want forbidden, got %+v", rerr)
		}
	})
}

// --- JSON-RPC plumbing ---

func dispatchStr(t *testing.T, srv *Server, sess *session, msg string) []byte {
	t.Helper()
	return srv.dispatch(context.Background(), sess, []byte(msg))
}

func rpcResponse(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if raw == nil {
		t.Fatal("want response, got nil (notification?)")
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return out
}

func TestJSONRPC(t *testing.T) {
	srv, st := newTestMCP(t)
	orgID := testOrg(t, st)
	memID := joinedEmployee(t, st, orgID, "E-1", mcpUniq("mem")+"@t.test", store.RoleMember)
	mintToken(t, st, orgID, "pat", "mem", "tok-mem", &memID, nil)

	t.Run("tools need initialize first on stateful sessions", func(t *testing.T) {
		sess := &session{server: srv, token: "tok-mem"}
		raw := dispatchStr(t, srv, sess,
			`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		out := rpcResponse(t, raw)
		errObj, _ := out["error"].(map[string]any)
		if errObj["code"] != float64(rpcNotInitialized) {
			t.Errorf("code = %v, want %d", errObj["code"], rpcNotInitialized)
		}
	})

	t.Run("initialize with token param", func(t *testing.T) {
		sess := &session{server: srv}
		raw := dispatchStr(t, srv, sess,
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{},"token":"tok-mem"}}`)
		out := rpcResponse(t, raw)
		res, _ := out["result"].(map[string]any)
		if res["protocolVersion"] != protocolVersion {
			t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], protocolVersion)
		}
		caps, _ := res["capabilities"].(map[string]any)
		if _, ok := caps["tools"]; !ok {
			t.Error("capabilities missing tools")
		}
		if _, ok := caps["elicitation"]; !ok {
			t.Error("capabilities missing elicitation")
		}
		if !sess.initialized {
			t.Error("session not marked initialized")
		}
	})

	t.Run("tools/list then tools/call round trip", func(t *testing.T) {
		sess := &session{server: srv, token: "tok-mem", stateless: true}
		raw := dispatchStr(t, srv, sess,
			`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
		tools, _ := rpcResponse(t, raw)["result"].(map[string]any)["tools"].([]any)
		if len(tools) != 37 {
			t.Fatalf("tools/list returned %d tools, want 37", len(tools))
		}
		raw = dispatchStr(t, srv, sess,
			`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hris.clock_in","arguments":{"occurred_at":"2026-10-16T09:00:00Z"}}}`)
		out := rpcResponse(t, raw)
		res, _ := out["result"].(map[string]any)
		if res["isError"] == true {
			t.Fatalf("tools/call failed: %v", res)
		}
	})

	t.Run("unknown tool is invalid params", func(t *testing.T) {
		sess := &session{server: srv, token: "tok-mem", stateless: true}
		raw := dispatchStr(t, srv, sess,
			`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"hris.nope","arguments":{}}}`)
		errObj, _ := rpcResponse(t, raw)["error"].(map[string]any)
		if errObj["code"] != float64(rpcInvalidParams) {
			t.Errorf("code = %v, want %d", errObj["code"], rpcInvalidParams)
		}
	})

	t.Run("unknown method", func(t *testing.T) {
		sess := &session{server: srv, token: "tok-mem", stateless: true}
		raw := dispatchStr(t, srv, sess,
			`{"jsonrpc":"2.0","id":5,"method":"resources/list","params":{}}`)
		errObj, _ := rpcResponse(t, raw)["error"].(map[string]any)
		if errObj["code"] != float64(rpcMethodNotFound) {
			t.Errorf("code = %v, want %d", errObj["code"], rpcMethodNotFound)
		}
	})

	t.Run("ping and notifications", func(t *testing.T) {
		sess := &session{server: srv}
		raw := dispatchStr(t, srv, sess, `{"jsonrpc":"2.0","id":6,"method":"ping"}`)
		if _, ok := rpcResponse(t, raw)["result"]; !ok {
			t.Error("ping should return a result")
		}
		if resp := srv.dispatch(context.Background(), sess,
			[]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); resp != nil {
			t.Error("notification should produce no response")
		}
		if !sess.initialized {
			t.Error("notifications/initialized should mark the session initialized")
		}
	})

	t.Run("elicitation stub", func(t *testing.T) {
		_, terr := srv.Elicit(context.Background(), "approve?", map[string]any{})
		if terr == nil || terr.Code != "ELICITATION_NOT_SUPPORTED" {
			t.Errorf("want ELICITATION_NOT_SUPPORTED, got %+v", terr)
		}
	})
}

// --- HTTP transport ---

type sseCapture struct {
	header  http.Header
	buf     bytes.Buffer
	flushed chan struct{}
}

func (w *sseCapture) Header() http.Header         { return w.header }
func (w *sseCapture) Write(b []byte) (int, error) { return w.buf.Write(b) }
func (w *sseCapture) WriteHeader(int)             {}
func (w *sseCapture) Flush() {
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func TestHTTPTransport(t *testing.T) {
	srv, st := newTestMCP(t)
	orgID := testOrg(t, st)
	memID := joinedEmployee(t, st, orgID, "E-1", mcpUniq("mem")+"@t.test", store.RoleMember)
	mintToken(t, st, orgID, "pat", "mem", "tok-mem", &memID, nil)
	h := srv.HTTPHandler()

	post := func(t *testing.T, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	t.Run("POST tools/call without token is unauthenticated", func(t *testing.T) {
		rr := post(t, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if out["error"].(map[string]any)["code"] != float64(rpcUnauthenticated) {
			t.Errorf("code = %v, want %d", out["error"].(map[string]any)["code"], rpcUnauthenticated)
		}
	})

	t.Run("POST tools/call round trip", func(t *testing.T) {
		rr := post(t, "tok-mem",
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hris.clock_in","arguments":{"occurred_at":"2026-10-16T09:00:00Z"}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		res, _ := out["result"].(map[string]any)
		if res["isError"] == true {
			t.Fatalf("tools/call failed: %v", res)
		}
	})

	t.Run("POST notification returns 202", func(t *testing.T) {
		rr := post(t, "tok-mem", `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		if rr.Code != http.StatusAccepted {
			t.Errorf("status = %d, want 202", rr.Code)
		}
	})

	t.Run("POST batch is rejected", func(t *testing.T) {
		rr := post(t, "tok-mem", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if out["error"].(map[string]any)["code"] != float64(rpcInvalidRequest) {
			t.Errorf("code = %v, want %d", out["error"].(map[string]any)["code"], rpcInvalidRequest)
		}
	})

	t.Run("GET /mcp emits ready SSE event", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer tok-mem")
		cw := &sseCapture{header: http.Header{}, flushed: make(chan struct{}, 1)}
		done := make(chan struct{})
		go func() { h.ServeHTTP(cw, req); close(done) }()
		select {
		case <-cw.flushed:
		case <-time.After(5 * time.Second):
			t.Fatal("no SSE flush within 5s")
		}
		cancel()
		<-done
		body := cw.buf.String()
		if !strings.HasPrefix(body, "event: ready\n") {
			t.Errorf("SSE stream does not start with ready event:\n%s", body)
		}
		if ct := cw.header.Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("Content-Type = %q, want text/event-stream", ct)
		}
	})
}
