package api

// Authentication and authorization unit tests: bearer-token failure modes,
// role ranking, scope sets, and the service-account effective-role rule.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// TestAuthenticateFailures: every way a request can fail authentication must
// produce 401 UNAUTHENTICATED with no information leak between the cases.
func TestAuthenticateFailures(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	a := bootstrap(t, srv, "authfail")
	orgPath := "/api/v1/organizations/" + a.Organization.ID
	probe := orgPath + "/employees"

	mkJoined := func(number, email string) (empID string) {
		emp, err := st.CreateEmployee(ctx, store.Employee{
			OrganizationID: a.Organization.ID,
			EmployeeNumber: number,
			FirstName:      "Auth",
			Email:          email,
			Status:         store.InvitationJoined,
		})
		if err != nil {
			t.Fatalf("CreateEmployee: %v", err)
		}
		return emp.ID
	}

	// Revoked token.
	revEmp := mkJoined("E-REV", "rev@example.test")
	mintToken(t, st, a.Organization.ID, "pat", "revoked", "tok-revoked-plain", &revEmp, nil)
	revCred, _, err := st.ListCredentials(ctx, a.Organization.ID, store.Page{Limit: 1000})
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	for _, c := range revCred {
		if c.Name == "revoked" {
			if err := st.RevokeCredential(ctx, a.Organization.ID, c.ID); err != nil {
				t.Fatalf("RevokeCredential: %v", err)
			}
		}
	}

	// Expired token (minted store-direct; the API rejects past expiries).
	expEmp := mkJoined("E-EXP", "exp@example.test")
	past := time.Now().Add(-time.Hour).UTC()
	if _, err := st.IssueCredential(ctx, store.Credential{
		OrganizationID: a.Organization.ID,
		Kind:           store.CredentialPAT,
		Name:           "expired",
		EmployeeID:     &expEmp,
		ExpiresAt:      &past,
	}, "tok-expired-plain"); err != nil {
		t.Fatalf("IssueCredential expired: %v", err)
	}

	// Token whose employee was deactivated after issuance.
	goneEmp := mkJoined("E-GONE", "gone@example.test")
	mintToken(t, st, a.Organization.ID, "pat", "gone", "tok-gone-plain", &goneEmp, nil)
	gone, err := st.GetEmployee(ctx, a.Organization.ID, goneEmp)
	if err != nil {
		t.Fatalf("GetEmployee: %v", err)
	}
	if _, err := st.DeactivateEmployee(ctx, a.Organization.ID, goneEmp, gone.Version); err != nil {
		t.Fatalf("DeactivateEmployee: %v", err)
	}

	// PAT bound to a never-joined employee must not authenticate either.
	pending, err := st.CreateEmployee(ctx, store.Employee{
		OrganizationID: a.Organization.ID,
		EmployeeNumber: "E-PEND",
		FirstName:      "Pending",
		Status:         store.InvitationNotInvited,
	})
	if err != nil {
		t.Fatalf("CreateEmployee pending: %v", err)
	}
	mintToken(t, st, a.Organization.ID, "pat", "pending", "tok-pending-plain", &pending.ID, nil)

	cases := []struct {
		name      string
		token     string // "" means no Authorization header at all
		rawHeader string // when set, used verbatim as the Authorization header
	}{
		{"no header", "", ""},
		{"garbage token", "hris_definitely-not-a-token", ""},
		{"wrong scheme", "", "Basic dXNlcjpwYXNz"},
		{"revoked token", "tok-revoked-plain", ""},
		{"expired token", "tok-expired-plain", ""},
		{"deactivated employee token", "tok-gone-plain", ""},
		{"never-joined employee token", "tok-pending-plain", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.rawHeader != "" {
				headers["Authorization"] = tc.rawHeader
			}
			req := doReq(t, srv, http.MethodGet, probe, tc.token, nil, headers)
			if req.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d: %s", req.Code, req.Body.String())
			}
		})
	}

	// Sanity: the bootstrap owner token still works.
	if rr := doReq(t, srv, http.MethodGet, probe, a.Token, nil, nil); rr.Code != http.StatusOK {
		t.Fatalf("owner token: want 200, got %d", rr.Code)
	}
}

// TestEffectiveRole covers the service-account role inference: a service
// account is treated as the highest role whose scope set it fully covers,
// and has no role at all when its scopes don't cover any role's set.
func TestEffectiveRole(t *testing.T) {
	managerScopes := scopesForRole(store.RoleManager)
	memberScopes := scopesForRole(store.RoleMember)

	cases := []struct {
		name string
		ai   *AuthInfo
		want store.Role
	}{
		{
			name: "PAT uses membership role",
			ai:   &AuthInfo{Kind: store.CredentialPAT, Role: store.RoleMember, Scopes: scopesForRole(store.RoleMember)},
			want: store.RoleMember,
		},
		{
			name: "service account with all scopes is owner-equivalent",
			ai:   &AuthInfo{Kind: store.CredentialServiceAccount, Scopes: store.AllScopes()},
			want: store.RoleOwner,
		},
		{
			name: "service account with exactly manager scopes is manager-equivalent",
			ai:   &AuthInfo{Kind: store.CredentialServiceAccount, Scopes: managerScopes},
			want: store.RoleManager,
		},
		{
			name: "service account with member scopes is member-equivalent",
			ai:   &AuthInfo{Kind: store.CredentialServiceAccount, Scopes: memberScopes},
			want: store.RoleMember,
		},
		{
			name: "service account with a single exotic scope has no role",
			ai:   &AuthInfo{Kind: store.CredentialServiceAccount, Scopes: []string{store.ScopePayrollSubmit}},
			want: store.Role(""),
		},
		{
			name: "service account with a subset of member scopes has no role",
			ai:   &AuthInfo{Kind: store.CredentialServiceAccount, Scopes: []string{store.ScopeTimeRead}},
			want: store.Role(""),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveRole(tc.ai); got != tc.want {
				t.Fatalf("want role %q, got %q", tc.want, got)
			}
		})
	}

	// The no-role service account can still pass pure scope gates but never
	// role gates.
	payrollOnly := &AuthInfo{Kind: store.CredentialServiceAccount, Scopes: []string{store.ScopePayrollSubmit}}
	if !requireScope(store.ScopePayrollSubmit, payrollOnly) {
		t.Fatalf("payroll:submit scope gate should pass")
	}
	if requireRole(store.RoleMember, payrollOnly) {
		t.Fatalf("no-role service account must not pass a role gate")
	}
}

// TestScopesForRole pins the component-1 scope contract.
func TestScopesForRole(t *testing.T) {
	for _, r := range []store.Role{store.RoleOwner, store.RoleAdmin} {
		got := scopesForRole(r)
		if len(got) != len(store.AllScopes()) {
			t.Fatalf("role %s: want all %d scopes, got %d", r, len(store.AllScopes()), len(got))
		}
	}
	mgr := scopesForRole(store.RoleManager)
	has := func(scopes []string, want string) bool {
		for _, s := range scopes {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(mgr, store.ScopeAuditRead) {
		t.Fatalf("manager should hold audit:read")
	}
	if has(mgr, store.ScopePayrollSubmit) {
		t.Fatalf("manager must not hold payroll:submit")
	}
	mem := scopesForRole(store.RoleMember)
	if has(mem, store.ScopeAuditRead) {
		t.Fatalf("member must not hold audit:read")
	}
	if !has(mem, store.ScopeEmployeesRead) {
		t.Fatalf("member should hold employees:read")
	}
	if scopesForRole(store.Role("bogus")) != nil {
		t.Fatalf("unknown role should map to no scopes")
	}
}

// TestRequireRoleRanking pins owner > admin > manager > member.
func TestRequireRoleRanking(t *testing.T) {
	ai := func(r store.Role) *AuthInfo {
		return &AuthInfo{Kind: store.CredentialPAT, Role: r, Scopes: scopesForRole(r)}
	}
	order := []store.Role{store.RoleMember, store.RoleManager, store.RoleAdmin, store.RoleOwner}
	for i, lower := range order {
		for j, higher := range order {
			want := j >= i
			if got := requireRole(lower, ai(higher)); got != want {
				t.Fatalf("requireRole(%s, %s): want %v, got %v", lower, higher, want, got)
			}
		}
	}
}

// TestBootstrapValidation: the public bootstrap rejects bad input without
// creating anything (no org, no employee, no token).
func TestBootstrapValidation(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	bad := []map[string]any{
		{}, // empty
		{"name": "X", "slug": "x", "country_code": "MY"}, // no owner
		{"name": "X", "slug": "x", "country_code": "XX",
			"owner": map[string]any{"first_name": "A", "last_name": "B", "email": "a@b.c"}}, // bad country
		{"name": "X", "slug": "x", "country_code": "MY",
			"owner": map[string]any{"first_name": "A", "last_name": "B", "email": "not-an-email"}}, // bad email
	}
	for i, body := range bad {
		rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations", "", body, nil)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("case %d: want 400, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}
	orgs, _, err := st.ListOrganizations(ctx, store.Page{})
	if err != nil || len(orgs) != 0 {
		t.Fatalf("failed bootstraps must not create orgs: %d (%v)", len(orgs), err)
	}

	// Duplicate slug on a valid bootstrap -> 409, exactly one org survives.
	good := map[string]any{
		"name": "Dup", "slug": "dup", "country_code": "ID",
		"owner": map[string]any{"first_name": "A", "last_name": "B", "email": "a@b.c"},
	}
	if rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations", "", good, nil); rr.Code != http.StatusCreated {
		t.Fatalf("first bootstrap: want 201, got %d", rr.Code)
	}
	if rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations", "", good, nil); rr.Code != http.StatusConflict {
		t.Fatalf("duplicate slug: want 409, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestEmployeesWriteScope: the employees:write scope grants least-privilege
// employee provisioning on REST, SCIM, and the MCP tool gate — without
// minting an all-scopes (admin-equivalent) service account.
func TestEmployeesWriteScope(t *testing.T) {
	srv, st := newTestServer(t)
	a := bootstrap(t, srv, "empwrite")
	orgID := a.Organization.ID
	empPath := "/api/v1/organizations/" + orgID + "/employees"

	newHire := func(tag string) map[string]any {
		return map[string]any{
			"first_name": "New",
			"last_name":  "Hire",
			"email":      "hire-" + tag + "@example.test",
		}
	}

	// Least-privilege service account: read + write, nothing else.
	mintToken(t, st, orgID, "service_account", "provisioner", "tok-provisioner", nil,
		[]string{store.ScopeEmployeesRead, store.ScopeEmployeesWrite})
	rr := doReq(t, srv, http.MethodPost, empPath, "tok-provisioner", newHire("a"), nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("REST create with employees:write: want 201, got %d: %s", rr.Code, rr.Body.String())
	}

	// Read-only service account still cannot create.
	mintToken(t, st, orgID, "service_account", "reader", "tok-reader", nil,
		[]string{store.ScopeEmployeesRead})
	rr = doReq(t, srv, http.MethodPost, empPath, "tok-reader", newHire("b"), nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("REST create with employees:read only: want 403, got %d", rr.Code)
	}

	// SCIM: write-scoped token provisions; read-only is denied.
	code, _ := scimCreateUser(t, srv, "tok-provisioner", "scim-w@example.test", "entra-w", nil)
	if code != http.StatusCreated {
		t.Fatalf("SCIM create with employees:write: want 201, got %d", code)
	}
	code, _ = scimCreateUser(t, srv, "tok-reader", "scim-r@example.test", "entra-r", nil)
	if code != http.StatusForbidden {
		t.Fatalf("SCIM create with employees:read only: want 403, got %d", code)
	}

	// Unknown scope is still rejected at issuance.
	if store.ValidScope("employees:delete") {
		t.Fatalf("employees:delete must not be a registered scope")
	}
	if !store.ValidScope(store.ScopeEmployeesWrite) {
		t.Fatalf("employees:write must be registered in AllScopes")
	}
}
