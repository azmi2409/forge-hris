package api

// SCIM 2.0 tests (forge-hris component 4, Worker C): the full user and group
// CRUD flows plus the auth matrix, against the MemoryStore via httptest.
// The Postgres adapter for scim_links is compile-checked only (no database
// in this environment).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/azmi2409/forge-hris/store"
)

// scimSetup bootstraps an org and mints a service-account token holding
// every scope (admin-equivalent, which SCIM mutations require). SCIM routes
// are registered by Server.routes().
func scimSetup(t *testing.T) (*Server, store.Store, string, string) {
	t.Helper()
	srv, st := newTestServer(t)
	boot := bootstrap(t, srv, "scim")
	adminToken := "scim-admin-token"
	mintToken(t, st, boot.Organization.ID, "service_account", "scim-provisioning", adminToken, nil, store.AllScopes())
	return srv, st, boot.Organization.ID, adminToken
}

func scimGet(t *testing.T, srv *Server, token, path string) (int, map[string]any) {
	t.Helper()
	rr := doReq(t, srv, http.MethodGet, path, token, nil, nil)
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: decode: %v (body %q)", path, err, rr.Body.String())
	}
	return rr.Code, body
}

func scimDo(t *testing.T, srv *Server, method, token, path string, payload any) (int, map[string]any) {
	t.Helper()
	rr := doReq(t, srv, method, path, token, payload, nil)
	var body map[string]any
	if rr.Body.Len() > 0 {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s %s: decode: %v (body %q)", method, path, err, rr.Body.String())
		}
	}
	return rr.Code, body
}

func mustSCIMUser(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	schemas, _ := body["schemas"].([]any)
	if len(schemas) != 1 || schemas[0] != "urn:ietf:params:scim:schemas:core:2.0:User" {
		t.Fatalf("wrong schemas: %v", body["schemas"])
	}
	return body
}

func scimCreateUser(t *testing.T, srv *Server, token, userName, externalID string, active *bool) (int, map[string]any) {
	t.Helper()
	payload := map[string]any{
		"schemas":    []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"userName":   userName,
		"name":       map[string]any{"givenName": "Ada", "familyName": "Lovelace"},
		"emails":     []any{map[string]any{"value": userName, "primary": true}},
		"externalId": externalID,
	}
	if active != nil {
		payload["active"] = *active
	}
	return scimDo(t, srv, http.MethodPost, token, "/scim/v2/Users", payload)
}

func TestSCIMUserLifecycle(t *testing.T) {
	srv, st, orgID, token := scimSetup(t)
	ctx := context.Background()
	ls := st.(store.SCIMLinkStore)

	// --- create ---
	code, created := scimCreateUser(t, srv, token, "ada@example.test", "entra-001", nil)
	if code != http.StatusCreated {
		t.Fatalf("create: want 201, got %d: %v", code, created)
	}
	mustSCIMUser(t, created)
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatalf("create: missing id: %v", created)
	}
	if created["userName"] != "ada@example.test" {
		t.Fatalf("create: userName = %v", created["userName"])
	}
	if created["externalId"] != "entra-001" {
		t.Fatalf("create: externalId = %v", created["externalId"])
	}
	if created["active"] != true {
		t.Fatalf("create: active = %v", created["active"])
	}
	name, _ := created["name"].(map[string]any)
	if name["givenName"] != "Ada" || name["familyName"] != "Lovelace" {
		t.Fatalf("create: name = %v", name)
	}

	// The employee exists in the store and the scim link was recorded.
	emp, err := st.GetEmployee(ctx, orgID, id)
	if err != nil {
		t.Fatalf("GetEmployee: %v", err)
	}
	if emp.Email != "ada@example.test" || emp.Status != store.InvitationJoined {
		t.Fatalf("employee = %+v", emp)
	}
	link, err := ls.GetSCIMLinkByExternalID(ctx, orgID, "entra-001")
	if err != nil {
		t.Fatalf("GetSCIMLinkByExternalID: %v", err)
	}
	if link.EmployeeID != id {
		t.Fatalf("link = %+v", link)
	}

	// --- get ---
	code, got := scimGet(t, srv, token, "/scim/v2/Users/"+id)
	if code != http.StatusOK {
		t.Fatalf("get: want 200, got %d: %v", code, got)
	}
	mustSCIMUser(t, got)

	// --- filter by userName ---
	code, listed := scimGet(t, srv, token, `/scim/v2/Users?filter=userName%20eq%20%22ada%40example.test%22`)
	if code != http.StatusOK {
		t.Fatalf("filter userName: want 200, got %d: %v", code, listed)
	}
	if listed["totalResults"] != float64(1) {
		t.Fatalf("filter userName: totalResults = %v", listed["totalResults"])
	}

	// --- filter by externalId ---
	code, listed = scimGet(t, srv, token, `/scim/v2/Users?filter=externalId%20eq%20%22entra-001%22`)
	if code != http.StatusOK {
		t.Fatalf("filter externalId: want 200, got %d: %v", code, listed)
	}
	if listed["totalResults"] != float64(1) {
		t.Fatalf("filter externalId: totalResults = %v", listed["totalResults"])
	}
	res, _ := listed["Resources"].([]any)
	if len(res) != 1 || res[0].(map[string]any)["id"] != id {
		t.Fatalf("filter externalId: Resources = %v", listed["Resources"])
	}

	// --- unknown externalId filter -> empty list ---
	code, listed = scimGet(t, srv, token, `/scim/v2/Users?filter=externalId%20eq%20%22nope%22`)
	if code != http.StatusOK || listed["totalResults"] != float64(0) {
		t.Fatalf("filter unknown: want 200/0, got %d: %v", code, listed)
	}

	// --- bad filter -> 400 SCIM error ---
	code, bad := scimGet(t, srv, token, `/scim/v2/Users?filter=emails%20co%20%22x%22`)
	if code != http.StatusBadRequest {
		t.Fatalf("bad filter: want 400, got %d: %v", code, bad)
	}
	if !strings.Contains(bad["schemas"].([]any)[0].(string), "scim:api:messages:2.0:Error") {
		t.Fatalf("bad filter: not a SCIM error: %v", bad)
	}

	// --- PUT replace ---
	code, replaced := scimDo(t, srv, http.MethodPut, token, "/scim/v2/Users/"+id, map[string]any{
		"userName":   "ada.new@example.test",
		"name":       map[string]any{"givenName": "Ada", "familyName": "Byron"},
		"externalId": "entra-001",
	})
	if code != http.StatusOK {
		t.Fatalf("put: want 200, got %d: %v", code, replaced)
	}
	if replaced["userName"] != "ada.new@example.test" {
		t.Fatalf("put: userName = %v", replaced["userName"])
	}
	if replaced["name"].(map[string]any)["familyName"] != "Byron" {
		t.Fatalf("put: name = %v", replaced["name"])
	}
	emp, _ = st.GetEmployee(ctx, orgID, id)
	if emp.Email != "ada.new@example.test" || emp.LastName != "Byron" {
		t.Fatalf("put: employee = %+v", emp)
	}

	// --- PATCH: rename + deactivate ---
	code, patched := scimDo(t, srv, http.MethodPatch, token, "/scim/v2/Users/"+id, map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []any{
			map[string]any{"op": "replace", "path": "name.familyName", "value": "King"},
			map[string]any{"op": "replace", "path": "active", "value": false},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("patch: want 200, got %d: %v", code, patched)
	}
	if patched["active"] != false {
		t.Fatalf("patch: active = %v", patched["active"])
	}
	emp, _ = st.GetEmployee(ctx, orgID, id)
	if emp.Status != store.InvitationRemoved || emp.LastName != "King" {
		t.Fatalf("patch: employee = %+v", emp)
	}

	// --- DELETE: soft-deactivate, record kept ---
	rr := doReq(t, srv, http.MethodDelete, "/scim/v2/Users/"+id, token, nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d: %s", rr.Code, rr.Body.String())
	}
	emp, err = st.GetEmployee(ctx, orgID, id)
	if err != nil {
		t.Fatalf("delete must not hard-delete: %v", err)
	}
	if emp.Status != store.InvitationRemoved {
		t.Fatalf("delete: status = %v", emp.Status)
	}
	// The scim link survives deactivation so the IdP can re-correlate.
	if _, err := ls.GetSCIMLinkByEmployee(ctx, orgID, id); err != nil {
		t.Fatalf("link must survive deactivation: %v", err)
	}
	// GET still returns the deactivated user with active=false.
	code, got = scimGet(t, srv, token, "/scim/v2/Users/"+id)
	if code != http.StatusOK || got["active"] != false {
		t.Fatalf("get after delete: want 200/active=false, got %d: %v", code, got)
	}

	// --- GET unknown id -> 404 SCIM error ---
	code, nf := scimGet(t, srv, token, "/scim/v2/Users/does-not-exist")
	if code != http.StatusNotFound {
		t.Fatalf("get unknown: want 404, got %d: %v", code, nf)
	}
	if nf["status"] != "404" {
		t.Fatalf("get unknown: status field = %v", nf["status"])
	}
}

func TestSCIMCreateInactive(t *testing.T) {
	srv, st, orgID, token := scimSetup(t)
	inactive := false
	code, created := scimCreateUser(t, srv, token, "grace@example.test", "entra-002", &inactive)
	if code != http.StatusCreated {
		t.Fatalf("create inactive: want 201, got %d: %v", code, created)
	}
	if created["active"] != false {
		t.Fatalf("create inactive: active = %v", created["active"])
	}
	id, _ := created["id"].(string)
	emp, err := st.GetEmployee(context.Background(), orgID, id)
	if err != nil {
		t.Fatalf("GetEmployee: %v", err)
	}
	if emp.Status != store.InvitationRemoved {
		t.Fatalf("create inactive: status = %v", emp.Status)
	}
}

func TestSCIMDuplicateExternalID(t *testing.T) {
	srv, _, _, token := scimSetup(t)
	code, _ := scimCreateUser(t, srv, token, "one@example.test", "entra-dup", nil)
	if code != http.StatusCreated {
		t.Fatalf("first create: want 201, got %d", code)
	}
	code, dup := scimCreateUser(t, srv, token, "two@example.test", "entra-dup", nil)
	if code != http.StatusConflict {
		t.Fatalf("dup externalId: want 409, got %d: %v", code, dup)
	}
	if dup["scimType"] != "uniqueness" {
		t.Fatalf("dup externalId: scimType = %v", dup["scimType"])
	}
	// Duplicate email -> 409 as well.
	code, dup = scimCreateUser(t, srv, token, "one@example.test", "entra-other", nil)
	if code != http.StatusConflict {
		t.Fatalf("dup email: want 409, got %d: %v", code, dup)
	}
}

func TestSCIMGroupLifecycle(t *testing.T) {
	srv, st, orgID, token := scimSetup(t)
	ctx := context.Background()

	// A provisioned member to put in the group.
	_, created := scimCreateUser(t, srv, token, "member@example.test", "entra-m1", nil)
	memberID, _ := created["id"].(string)

	// --- create group with members ---
	code, group := scimDo(t, srv, http.MethodPost, token, "/scim/v2/Groups", map[string]any{
		"schemas":     []string{"urn:ietf:params:scim:schemas:core:2.0:Group"},
		"displayName": "Engineering",
		"members":     []any{map[string]any{"value": memberID}},
	})
	if code != http.StatusCreated {
		t.Fatalf("group create: want 201, got %d: %v", code, group)
	}
	schemas, _ := group["schemas"].([]any)
	if len(schemas) != 1 || schemas[0] != "urn:ietf:params:scim:schemas:core:2.0:Group" {
		t.Fatalf("group create: schemas = %v", group["schemas"])
	}
	groupID, _ := group["id"].(string)
	members, _ := group["members"].([]any)
	if len(members) != 1 || members[0].(map[string]any)["value"] != memberID {
		t.Fatalf("group create: members = %v", group["members"])
	}
	// Membership is the employee's group assignment.
	emp, _ := st.GetEmployee(ctx, orgID, memberID)
	if emp.GroupID == nil || *emp.GroupID != groupID {
		t.Fatalf("member not assigned: %+v", emp.GroupID)
	}

	// --- get group ---
	code, got := scimGet(t, srv, token, "/scim/v2/Groups/"+groupID)
	if code != http.StatusOK {
		t.Fatalf("group get: want 200, got %d: %v", code, got)
	}

	// --- list groups ---
	code, listed := scimGet(t, srv, token, "/scim/v2/Groups")
	if code != http.StatusOK || listed["totalResults"] != float64(1) {
		t.Fatalf("group list: want 200/1, got %d: %v", code, listed)
	}

	// --- PATCH: remove member ---
	code, patched := scimDo(t, srv, http.MethodPatch, token, "/scim/v2/Groups/"+groupID, map[string]any{
		"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []any{
			map[string]any{"op": "remove", "path": "members", "value": []any{map[string]any{"value": memberID}}},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("group patch remove: want 200, got %d: %v", code, patched)
	}
	if len(patched["members"].([]any)) != 0 {
		t.Fatalf("group patch remove: members = %v", patched["members"])
	}
	emp, _ = st.GetEmployee(ctx, orgID, memberID)
	if emp.GroupID != nil {
		t.Fatalf("member not unassigned: %+v", *emp.GroupID)
	}

	// --- PATCH: add member back + rename ---
	code, patched = scimDo(t, srv, http.MethodPatch, token, "/scim/v2/Groups/"+groupID, map[string]any{
		"Operations": []any{
			map[string]any{"op": "add", "path": "members", "value": []any{map[string]any{"value": memberID}}},
			map[string]any{"op": "replace", "path": "displayName", "value": "Platform"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("group patch add: want 200, got %d: %v", code, patched)
	}
	if patched["displayName"] != "Platform" || len(patched["members"].([]any)) != 1 {
		t.Fatalf("group patch add: %v", patched)
	}

	// --- DELETE with members still assigned -> 409 ---
	rr := doReq(t, srv, http.MethodDelete, "/scim/v2/Groups/"+groupID, token, nil, nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("group delete with members: want 409, got %d: %s", rr.Code, rr.Body.String())
	}

	// --- unassign, then DELETE -> 204 ---
	scimDo(t, srv, http.MethodPatch, token, "/scim/v2/Groups/"+groupID, map[string]any{
		"Operations": []any{
			map[string]any{"op": "remove", "path": "members", "value": []any{map[string]any{"value": memberID}}},
		},
	})
	rr = doReq(t, srv, http.MethodDelete, "/scim/v2/Groups/"+groupID, token, nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("group delete: want 204, got %d: %s", rr.Code, rr.Body.String())
	}
	code, nf := scimGet(t, srv, token, "/scim/v2/Groups/"+groupID)
	if code != http.StatusNotFound {
		t.Fatalf("group get after delete: want 404, got %d: %v", code, nf)
	}
}

func TestSCIMAuth(t *testing.T) {
	srv, st, orgID, adminToken := scimSetup(t)

	// --- no token -> 401 ---
	rr := doReq(t, srv, http.MethodGet, "/scim/v2/Users", "", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rr.Code)
	}

	// --- PAT token -> 401 with SCIM error body ---
	empID := joinedEmployee(t, st, orgID, "EMP-PAT", "pat@example.test", store.RoleAdmin)
	mintToken(t, st, orgID, "pat", "human", "scim-pat-token", &empID, nil)
	rr = doReq(t, srv, http.MethodGet, "/scim/v2/Users", "scim-pat-token", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("PAT: want 401, got %d: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "urn:ietf:params:scim:api:messages:2.0:Error") {
		t.Fatalf("PAT: not a SCIM error body: %s", rr.Body.String())
	}

	// --- service account without employees:read -> 403 ---
	mintToken(t, st, orgID, "service_account", "time-only", "scim-time-token", nil, []string{store.ScopeTimeRead})
	rr = doReq(t, srv, http.MethodGet, "/scim/v2/Users", "scim-time-token", nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("no employees:read: want 403, got %d: %s", rr.Code, rr.Body.String())
	}

	// --- service account with only employees:read: reads OK, writes 403 ---
	mintToken(t, st, orgID, "service_account", "reader", "scim-reader-token", nil, []string{store.ScopeEmployeesRead})
	code, _ := scimGet(t, srv, "scim-reader-token", "/scim/v2/Users")
	if code != http.StatusOK {
		t.Fatalf("read with employees:read: want 200, got %d", code)
	}
	code, denied := scimCreateUser(t, srv, "scim-reader-token", "x@example.test", "entra-x", nil)
	if code != http.StatusForbidden {
		t.Fatalf("write with employees:read only: want 403, got %d: %v", code, denied)
	}

	// --- bogus token -> 401 ---
	rr = doReq(t, srv, http.MethodGet, "/scim/v2/Users", "bogus", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bogus token: want 401, got %d", rr.Code)
	}

	// --- admin-equivalent service account still works (sanity) ---
	code, _ = scimGet(t, srv, adminToken, "/scim/v2/Users")
	if code != http.StatusOK {
		t.Fatalf("admin-equivalent read: want 200, got %d", code)
	}
}
