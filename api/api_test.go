package api

// API-level tests over httptest: the RBAC matrix (role x endpoint), the
// idempotency-key concurrency contract, cursor pagination walks, and the
// bootstrap -> lifecycle end-to-end. The store underneath is the MemoryStore;
// the Postgres adapter is covered by store/postgres_test.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/azmi2409/forge-hris/store"
)

// newTestServer builds a server on a fresh MemoryStore. The per-credential
// rate limiter is raised out of the way: tests legitimately make hundreds of
// requests on one credential (pagination walks), and rate limiting is a
// deployment concern, not an authorization one.
func newTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st := store.NewMemoryStore()
	srv := NewServer(st, nil)
	srv.limiter = newRateLimiter(1 << 30)
	return srv, st
}

func doReq(t *testing.T, srv *Server, method, path, token string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	return rr
}

type bootstrapOut struct {
	Organization struct {
		ID string `json:"id"`
	} `json:"organization"`
	Employee struct {
		ID string `json:"id"`
	} `json:"employee"`
	Token string `json:"token"`
}

func bootstrap(t *testing.T, srv *Server, slug string) bootstrapOut {
	t.Helper()
	rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations", "", map[string]any{
		"name":         "Test Co " + slug,
		"slug":         slug,
		"country_code": "MY",
		"owner": map[string]any{
			"first_name": "Own",
			"last_name":  "Er",
			"email":      "owner@" + slug + ".test",
		},
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("bootstrap %s: want 201, got %d: %s", slug, rr.Code, rr.Body.String())
	}
	var out bootstrapOut
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("bootstrap %s: decode: %v", slug, err)
	}
	if out.Token == "" || out.Organization.ID == "" || out.Employee.ID == "" {
		t.Fatalf("bootstrap %s: incomplete response: %+v", slug, out)
	}
	return out
}

// joinedEmployee creates a joined employee with the given role via the store
// (the API has no "invite/accept" flow in component 1).
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

// mintToken issues a credential via the store and returns the plaintext the
// API will accept as a bearer token.
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

var uniqCounter atomic.Int64

func uniqEmail(prefix string) string {
	return fmt.Sprintf("%s-%d@example.test", prefix, uniqCounter.Add(1))
}

// rbacFixture holds one org plus a token per role, a second org for
// cross-tenant checks, and the store for direct setup.
type rbacFixture struct {
	srv    *Server
	st     store.Store
	orgID  string
	tokens map[string]string
}

func newRBACFixture(t *testing.T, srv *Server, st store.Store) *rbacFixture {
	t.Helper()
	a := bootstrap(t, srv, "rbac-a")
	b := bootstrap(t, srv, "rbac-b")

	tokens := map[string]string{
		"owner": a.Token,
		"cross": b.Token,
		"none":  "",
		"bad":   "hris_this-is-not-a-real-token",
	}
	for _, r := range []struct {
		label string
		role  store.Role
	}{
		{"admin", store.RoleAdmin},
		{"manager", store.RoleManager},
		{"member", store.RoleMember},
	} {
		empID := joinedEmployee(t, st, a.Organization.ID, "E-"+r.label, uniqEmail(r.label), r.role)
		plain := "tok-" + r.label + "-plaintext"
		mintToken(t, st, a.Organization.ID, "pat", r.label+" token", plain, &empID, nil)
		tokens[r.label] = plain
	}
	return &rbacFixture{srv: srv, st: st, orgID: a.Organization.ID, tokens: tokens}
}

type rbacCase struct {
	name string
	// run executes the request for every label and returns actual statuses.
	run  func(t *testing.T, f *rbacFixture) map[string]int
	want map[string]int
}

func statusOf(t *testing.T, f *rbacFixture, method, path, label string, body any) int {
	t.Helper()
	return doReq(t, f.srv, method, path, f.tokens[label], body, nil).Code
}

func allLabels(f *rbacFixture, fn func(label string) int) map[string]int {
	got := make(map[string]int, len(f.tokens))
	for label := range f.tokens {
		got[label] = fn(label)
	}
	return got
}

func TestRBACMatrix(t *testing.T) {
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	orgPath := "/api/v1/organizations/" + f.orgID

	// freshGroup creates a group via the owner token and returns its ID.
	freshGroup := func(t *testing.T) string {
		rr := doReq(t, srv, http.MethodPost, orgPath+"/groups", f.tokens["owner"],
			map[string]any{"name": fmt.Sprintf("grp-%d", uniqCounter.Add(1))}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("setup group: %d %s", rr.Code, rr.Body.String())
		}
		var g struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &g); err != nil {
			t.Fatalf("setup group decode: %v", err)
		}
		return g.ID
	}
	// freshEmployee creates a plain (not_invited) employee via the owner.
	freshEmployee := func(t *testing.T) (id string, version int64) {
		rr := doReq(t, srv, http.MethodPost, orgPath+"/employees", f.tokens["owner"], map[string]any{
			"first_name": "Fresh",
			"last_name":  "Hire",
			"email":      uniqEmail("hire"),
		}, nil)
		if rr.Code != http.StatusCreated {
			t.Fatalf("setup employee: %d %s", rr.Code, rr.Body.String())
		}
		var e struct {
			ID      string `json:"id"`
			Version int64  `json:"version"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil {
			t.Fatalf("setup employee decode: %v", err)
		}
		return e.ID, e.Version
	}
	// freshJoined creates a joined employee (role grantable) via the store.
	freshJoined := func(t *testing.T) string {
		n := uniqCounter.Add(1)
		return joinedEmployee(t, st, f.orgID,
			fmt.Sprintf("EJ-%d", n), fmt.Sprintf("joined-%d@example.test", n), store.RoleMember)
	}
	// freshCredential mints a service-account credential via the store.
	freshCredential := func(t *testing.T) string {
		n := uniqCounter.Add(1)
		cred, err := st.IssueCredential(context.Background(), store.Credential{
			OrganizationID: f.orgID,
			Kind:           store.CredentialServiceAccount,
			Name:           fmt.Sprintf("svc-%d", n),
			Scopes:         []string{store.ScopeTimeRead},
		}, fmt.Sprintf("plaintext-svc-%d", n))
		if err != nil {
			t.Fatalf("setup credential: %v", err)
		}
		return cred.ID
	}

	// wantFull builds the standard expectation map.
	full := func(owner, admin, manager, member, none, bad, cross int) map[string]int {
		return map[string]int{
			"owner": owner, "admin": admin, "manager": manager, "member": member,
			"none": none, "bad": bad, "cross": cross,
		}
	}
	ok200 := full(200, 200, 200, 200, 401, 401, 403)
	adminOnly := full(200, 200, 403, 403, 401, 401, 403)
	managerUp := full(200, 200, 200, 403, 401, 401, 403)

	cases := []rbacCase{
		{
			name: "GET /healthz is public",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, "/healthz", label, nil)
				})
			},
			want: full(200, 200, 200, 200, 200, 200, 200),
		},
		{
			name: "GET /api/v1/organizations returns own org",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, "/api/v1/organizations", label, nil)
				})
			},
			// cross-tenant token gets its own org: 200, not 403.
			want: full(200, 200, 200, 200, 401, 401, 200),
		},
		{
			name: "GET /api/v1/organizations/{id}",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, orgPath, label, nil)
				})
			},
			want: ok200,
		},
		{
			name: "PATCH /api/v1/organizations/{id} requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					if label == "none" || label == "bad" {
						return statusOf(t, f, http.MethodPatch, orgPath, label,
							map[string]any{"name": "X", "version": 1})
					}
					// Fetch a fresh version per label: owner/admin mutate it.
					rr := doReq(t, f.srv, http.MethodGet, orgPath, f.tokens[label], nil, nil)
					if rr.Code != 200 {
						return rr.Code // cross -> 403 here
					}
					var org struct {
						Version int64 `json:"version"`
					}
					if err := json.Unmarshal(rr.Body.Bytes(), &org); err != nil {
						t.Fatalf("decode org: %v", err)
					}
					return statusOf(t, f, http.MethodPatch, orgPath, label,
						map[string]any{"name": "Renamed", "version": org.Version})
				})
			},
			want: adminOnly,
		},
		{
			name: "POST groups requires manager",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodPost, orgPath+"/groups", label,
						map[string]any{"name": fmt.Sprintf("g-%s-%d", label, uniqCounter.Add(1))})
				})
			},
			want: full(201, 201, 201, 403, 401, 401, 403),
		},
		{
			name: "GET groups",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, orgPath+"/groups", label, nil)
				})
			},
			want: ok200,
		},
		{
			name: "GET group/{id}",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				gid := freshGroup(t)
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, orgPath+"/groups/"+gid, label, nil)
				})
			},
			want: ok200,
		},
		{
			name: "PATCH group/{id} requires manager",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					gid := freshGroup(t)
					return statusOf(t, f, http.MethodPatch, orgPath+"/groups/"+gid, label,
						map[string]any{"name": "Renamed", "version": 1})
				})
			},
			want: managerUp,
		},
		{
			name: "DELETE group/{id} requires manager",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					gid := freshGroup(t)
					return statusOf(t, f, http.MethodDelete, orgPath+"/groups/"+gid, label, nil)
				})
			},
			want: full(204, 204, 204, 403, 401, 401, 403),
		},
		{
			name: "POST employees requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodPost, orgPath+"/employees", label, map[string]any{
						"first_name": "New",
						"last_name":  "Hire",
						"email":      uniqEmail("emp-" + label),
					})
				})
			},
			want: full(201, 201, 403, 403, 401, 401, 403),
		},
		{
			name: "GET employees (employees:read held by all roles)",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, orgPath+"/employees", label, nil)
				})
			},
			want: ok200,
		},
		{
			name: "GET employees/{id} of another employee",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				eid, _ := freshEmployee(t)
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, orgPath+"/employees/"+eid, label, nil)
				})
			},
			// members may only read their own record.
			want: full(200, 200, 200, 403, 401, 401, 403),
		},
		{
			name: "PATCH employees/{id} requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					eid, ver := freshEmployee(t)
					return statusOf(t, f, http.MethodPatch, orgPath+"/employees/"+eid, label,
						map[string]any{"position": "Engineer", "version": ver})
				})
			},
			want: adminOnly,
		},
		{
			name: "POST employees/{id}/deactivate requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					eid, ver := freshEmployee(t)
					return statusOf(t, f, http.MethodPost, orgPath+"/employees/"+eid+"/deactivate", label,
						map[string]any{"version": ver})
				})
			},
			want: adminOnly,
		},
		{
			name: "PUT roles/{employeeID} requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					eid := freshJoined(t)
					return statusOf(t, f, http.MethodPut, orgPath+"/roles/"+eid, label,
						map[string]any{"role": "member"})
				})
			},
			want: adminOnly,
		},
		{
			name: "GET roles requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, orgPath+"/roles", label, nil)
				})
			},
			want: adminOnly,
		},
		{
			name: "POST credentials requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodPost, orgPath+"/credentials", label, map[string]any{
						"kind":   "service_account",
						"name":   fmt.Sprintf("svc-%s-%d", label, uniqCounter.Add(1)),
						"scopes": []string{store.ScopeTimeRead},
					})
				})
			},
			want: full(201, 201, 403, 403, 401, 401, 403),
		},
		{
			name: "GET credentials requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, orgPath+"/credentials", label, nil)
				})
			},
			want: adminOnly,
		},
		{
			name: "DELETE credentials/{id} requires admin",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					cid := freshCredential(t)
					return statusOf(t, f, http.MethodDelete, orgPath+"/credentials/"+cid, label, nil)
				})
			},
			want: full(204, 204, 403, 403, 401, 401, 403),
		},
		{
			name: "GET audit_events requires audit:read",
			run: func(t *testing.T, f *rbacFixture) map[string]int {
				return allLabels(f, func(label string) int {
					return statusOf(t, f, http.MethodGet, orgPath+"/audit_events", label, nil)
				})
			},
			// owner/admin hold all scopes; managers hold audit:read; members do not.
			want: full(200, 200, 200, 403, 401, 401, 403),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.run(t, f)
			for label, want := range tc.want {
				if got[label] != want {
					t.Errorf("%s: label %q: want %d, got %d", tc.name, label, want, got[label])
				}
			}
		})
	}
}

// TestMemberCanReadOwnRecord: the one exception to the 403 above — a member
// reading their own employee record gets 200.
func TestMemberCanReadOwnRecord(t *testing.T) {
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	orgPath := "/api/v1/organizations/" + f.orgID

	memberEmpID := joinedEmployee(t, st, f.orgID, "E-SELF", uniqEmail("self"), store.RoleMember)
	memberTok := "tok-self-plaintext"
	mintToken(t, st, f.orgID, "pat", "self token", memberTok, &memberEmpID, nil)

	rr := doReq(t, srv, http.MethodGet, orgPath+"/employees/"+memberEmpID, memberTok, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("member reading own record: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAdminCannotPromoteToAdmin: admins may assign manager/member only; the
// owner may assign any role.
func TestAdminCannotPromoteToAdmin(t *testing.T) {
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	orgPath := "/api/v1/organizations/" + f.orgID

	target := joinedEmployee(t, st, f.orgID, "E-PROMO", uniqEmail("promo"), store.RoleMember)

	rr := doReq(t, srv, http.MethodPut, orgPath+"/roles/"+target, f.tokens["admin"],
		map[string]any{"role": "admin"}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("admin assigning admin: want 403, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, srv, http.MethodPut, orgPath+"/roles/"+target, f.tokens["owner"],
		map[string]any{"role": "admin"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("owner assigning admin: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestIdempotencyConcurrentReplay: two concurrent POSTs with the same
// Idempotency-Key execute once (second replays); a later same-key/same-body
// POST replays; same-key/different-body is rejected with 409.
func TestIdempotencyConcurrentReplay(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	a := bootstrap(t, srv, "idem")
	empPath := "/api/v1/organizations/" + a.Organization.ID + "/employees"
	keyHeader := map[string]string{"Idempotency-Key": "emp-key-1"}
	body := map[string]any{
		"employee_number": "IDEM-001",
		"first_name":      "Idem",
		"last_name":       "Potent",
		"email":           "idem1@example.test",
	}
	empIDOf := func(t *testing.T, rr *httptest.ResponseRecorder) string {
		t.Helper()
		var e struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &e); err != nil || e.ID == "" {
			t.Fatalf("decode employee id: %v body=%s", err, rr.Body.String())
		}
		return e.ID
	}

	var wg sync.WaitGroup
	resps := make([]*httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resps[i] = doReq(t, srv, http.MethodPost, empPath, a.Token, body, keyHeader)
		}(i)
	}
	wg.Wait()
	for i, rr := range resps {
		if rr.Code != http.StatusCreated {
			t.Fatalf("concurrent POST %d: want 201, got %d: %s", i, rr.Code, rr.Body.String())
		}
	}
	if id0, id1 := empIDOf(t, resps[0]), empIDOf(t, resps[1]); id0 != id1 {
		t.Fatalf("same idempotency key created two employees: %s vs %s", id0, id1)
	}
	emps, _, err := st.ListEmployees(ctx, a.Organization.ID,
		store.EmployeeFilter{Search: "IDEM-001"}, store.Page{})
	if err != nil || len(emps) != 1 {
		t.Fatalf("want exactly 1 employee IDEM-001, got %d (%v)", len(emps), err)
	}

	// Replay after the fact: same key + same body -> same employee, no dup.
	rr := doReq(t, srv, http.MethodPost, empPath, a.Token, body, keyHeader)
	if rr.Code != http.StatusCreated || empIDOf(t, rr) != empIDOf(t, resps[0]) {
		t.Fatalf("replay: want 201 with same id, got %d: %s", rr.Code, rr.Body.String())
	}
	emps, _, _ = st.ListEmployees(ctx, a.Organization.ID,
		store.EmployeeFilter{Search: "IDEM-001"}, store.Page{})
	if len(emps) != 1 {
		t.Fatalf("replay created a duplicate: %d employees", len(emps))
	}

	// Same key, different body -> 409 IDEMPOTENCY_KEY_REUSED.
	other := map[string]any{
		"employee_number": "IDEM-002",
		"first_name":      "Idem",
		"last_name":       "Potent",
		"email":           "idem2@example.test",
	}
	rr = doReq(t, srv, http.MethodPost, empPath, a.Token, other, keyHeader)
	if rr.Code != http.StatusConflict {
		t.Fatalf("key reuse with different body: want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Code != "IDEMPOTENCY_KEY_REUSED" {
		t.Fatalf("want code IDEMPOTENCY_KEY_REUSED, got %v body=%s", err, rr.Body.String())
	}
}

// TestEmployeePaginationWalk seeds 250 employees and walks the cursor pages,
// asserting no duplicates, no gaps, and correct has_more/next_cursor semantics.
func TestEmployeePaginationWalk(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	a := bootstrap(t, srv, "pagewalk")
	empPath := "/api/v1/organizations/" + a.Organization.ID + "/employees"

	const seeded = 250
	for i := 0; i < seeded; i++ {
		if _, err := st.CreateEmployee(ctx, store.Employee{
			OrganizationID: a.Organization.ID,
			EmployeeNumber: fmt.Sprintf("P-%04d", i),
			FirstName:      "Page",
			LastName:       "Walker",
			Email:          fmt.Sprintf("pw-%04d@example.test", i),
		}); err != nil {
			t.Fatalf("seed employee %d: %v", i, err)
		}
	}
	wantTotal := seeded + 1 // plus the bootstrap owner

	type pageOut struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		NextCursor string `json:"next_cursor"`
		HasMore    bool   `json:"has_more"`
	}

	seen := make(map[string]bool, wantTotal)
	cursor := ""
	pages := 0
	for {
		pages++
		if pages > 50 {
			t.Fatalf("pagination did not terminate after 50 pages")
		}
		path := empPath + "?limit=37"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		rr := doReq(t, srv, http.MethodGet, path, a.Token, nil, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("page %d: want 200, got %d: %s", pages, rr.Code, rr.Body.String())
		}
		var p pageOut
		if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
			t.Fatalf("page %d: decode: %v", pages, err)
		}
		if len(p.Data) == 0 {
			t.Fatalf("page %d: empty data with has_more=%v", pages, p.HasMore)
		}
		if p.HasMore != (p.NextCursor != "") {
			t.Fatalf("page %d: has_more=%v but next_cursor=%q", pages, p.HasMore, p.NextCursor)
		}
		for _, e := range p.Data {
			if seen[e.ID] {
				t.Fatalf("duplicate employee %s across pages", e.ID)
			}
			seen[e.ID] = true
		}
		if !p.HasMore {
			break
		}
		cursor = p.NextCursor
	}
	if len(seen) != wantTotal {
		t.Fatalf("walk collected %d employees, want %d (gap or dupe)", len(seen), wantTotal)
	}
	t.Logf("walked %d employees in %d pages", len(seen), pages)
}

// TestBootstrapLifecycle is the component-1 happy path end to end:
// bootstrap -> create employee -> grant role -> issue PAT -> authed request ->
// deactivate -> token stops working (401).
func TestBootstrapLifecycle(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	a := bootstrap(t, srv, "lifecycle")
	orgPath := "/api/v1/organizations/" + a.Organization.ID

	// Create an employee via the API.
	rr := doReq(t, srv, http.MethodPost, orgPath+"/employees", a.Token, map[string]any{
		"first_name": "E2E",
		"last_name":  "Case",
		"email":      "e2e@example.test",
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create employee: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}

	// Join them (store-level; component 1 has no invite flow) so a role can
	// be granted.
	emp, err := st.GetEmployee(ctx, a.Organization.ID, created.ID)
	if err != nil {
		t.Fatalf("GetEmployee: %v", err)
	}
	emp.Status = store.InvitationJoined
	if _, err := st.UpdateEmployee(ctx, emp); err != nil {
		t.Fatalf("join employee: %v", err)
	}

	// Grant a role.
	rr = doReq(t, srv, http.MethodPut, orgPath+"/roles/"+created.ID, a.Token,
		map[string]any{"role": "member"}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("set role: want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Issue a PAT for the new hire.
	rr = doReq(t, srv, http.MethodPost, orgPath+"/credentials", a.Token, map[string]any{
		"kind":        "pat",
		"name":        "e2e laptop",
		"employee_id": created.ID,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue PAT: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var issued struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &issued); err != nil || issued.Token == "" {
		t.Fatalf("decode issued token: %v", err)
	}

	// The new token authorizes requests.
	rr = doReq(t, srv, http.MethodGet, orgPath+"/employees", issued.Token, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("authed request: want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// Deactivate the employee (needs the current version).
	rr = doReq(t, srv, http.MethodGet, orgPath+"/employees/"+created.ID, a.Token, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get employee: want 200, got %d", rr.Code)
	}
	var cur struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &cur); err != nil {
		t.Fatalf("decode employee: %v", err)
	}
	rr = doReq(t, srv, http.MethodPost, orgPath+"/employees/"+created.ID+"/deactivate", a.Token,
		map[string]any{"version": cur.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("deactivate: want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	// The deactivated employee's token must stop working immediately.
	rr = doReq(t, srv, http.MethodGet, orgPath+"/employees", issued.Token, nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("deactivated token: want 401, got %d: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Code != "UNAUTHENTICATED" {
		t.Fatalf("want code UNAUTHENTICATED, got %v body=%s", err, rr.Body.String())
	}
}
