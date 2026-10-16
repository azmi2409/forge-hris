package api

// Xero route tests over httptest: the RBAC matrix on every route, the full
// OAuth flow (connect -> callback -> status -> mappings -> disconnect),
// bill posting (dry-run, live, idempotent), the 501 when the integration
// is not configured, and the auto-post hook firing on payroll submit.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/integrations/xero"
	"github.com/azmi2409/forge-hris/store"
)

var xeroTestSeq atomic.Int64

// newXeroTestServer builds a server with the Xero integration wired to a
// fake client, plus a bootstrapped org (owner token) and a member token.
func newXeroTestServer(t *testing.T, fake *xero.FakeClient) (*Server, store.Store, bootstrapOut, string) {
	t.Helper()
	t.Setenv("FORGE_HRIS_SECRET_KEY", "0123456789abcdef0123456789abcdef")
	srv, st := newTestServer(t)
	srv.SetXeroIntegration(xero.NewHandler(st.(xero.Store), xero.Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "https://app.example.test/api/v1/integrations/xero/callback",
		Client:       fake,
	}))
	a := bootstrap(t, srv, fmt.Sprintf("xero-%d", xeroTestSeq.Add(1)))
	memberID := joinedEmployee(t, st, a.Organization.ID, "X-1", uniqEmail("xero-member"), store.RoleMember)
	memberTok := "xero-member-token-" + a.Organization.ID
	mintToken(t, st, a.Organization.ID, "pat", "member token", memberTok, &memberID, nil)
	return srv, st, a, memberTok
}

func xeroBase(orgID string) string {
	return "/api/v1/organizations/" + orgID + "/integrations/xero"
}

// connectViaAPI runs the OAuth connect + callback flow through the routes
// and returns when the org is connected.
func connectViaAPI(t *testing.T, srv *Server, fake *xero.FakeClient, orgID, token string) {
	t.Helper()
	rr := doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/connect", token, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("connect: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var connOut struct {
		AuthorizationURL string `json:"authorization_url"`
		State            string `json:"state"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &connOut); err != nil || connOut.State == "" {
		t.Fatalf("connect decode: %v: %s", err, rr.Body.String())
	}
	if !strings.Contains(connOut.AuthorizationURL, "login.xero.com") {
		t.Fatalf("authorization_url: %s", connOut.AuthorizationURL)
	}
	// The callback needs no bearer token: Xero redirects the browser here.
	req := "/api/v1/integrations/xero/callback?code=auth-code-1&state=" + connOut.State
	rr = doReq(t, srv, http.MethodGet, req, "", nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("callback: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestXeroConnectRBAC(t *testing.T) {
	fake := &xero.FakeClient{}
	srv, _, a, memberTok := newXeroTestServer(t, fake)
	orgID := a.Organization.ID

	// Unauthenticated.
	rr := doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/connect", "", nil, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", rr.Code)
	}
	// Member is forbidden.
	rr = doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/connect", memberTok, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member: want 403, got %d: %s", rr.Code, rr.Body.String())
	}
	// Cross-org is forbidden.
	other := bootstrap(t, srv, fmt.Sprintf("xero-%d", xeroTestSeq.Add(1)))
	rr = doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/connect", other.Token, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-org: want 403, got %d", rr.Code)
	}
	// Admin works.
	rr = doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/connect", a.Token, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestXeroNotConfigured(t *testing.T) {
	srv, _ := newTestServer(t) // no SetXeroIntegration
	a := bootstrap(t, srv, fmt.Sprintf("xero-%d", xeroTestSeq.Add(1)))

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, xeroBase(a.Organization.ID) + "/connect"},
		{http.MethodGet, xeroBase(a.Organization.ID) + "/status"},
		{http.MethodPost, xeroBase(a.Organization.ID) + "/mappings"},
		{http.MethodDelete, xeroBase(a.Organization.ID)},
	} {
		rr := doReq(t, srv, tc.method, tc.path, a.Token, nil, nil)
		if rr.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s: want 501, got %d", tc.method, tc.path, rr.Code)
		}
		var out struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.Code != "XERO_NOT_CONFIGURED" {
			t.Fatalf("code: %v %v", out, err)
		}
	}
}

func TestXeroFullOAuthFlow(t *testing.T) {
	fake := &xero.FakeClient{
		ExchangeTokens: xero.Tokens{AccessToken: "at-1", RefreshToken: "rt-1",
			ExpiresAt: time.Now().Add(time.Hour)},
		Tenants: []xero.Tenant{{TenantID: "tenant-1"}},
	}
	srv, st, a, memberTok := newXeroTestServer(t, fake)
	orgID := a.Organization.ID

	// Status before connecting.
	rr := doReq(t, srv, http.MethodGet, xeroBase(orgID)+"/status", memberTok, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rr.Code)
	}
	var statusOut struct {
		Connected bool `json:"connected"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &statusOut); err != nil || statusOut.Connected {
		t.Fatalf("unconnected status: %s", rr.Body.String())
	}

	connectViaAPI(t, srv, fake, orgID, a.Token)

	// Status after connecting: any member can read it, tokens never leak.
	rr = doReq(t, srv, http.MethodGet, xeroBase(orgID)+"/status", memberTok, nil, nil)
	var full struct {
		Connected      bool   `json:"connected"`
		TenantID       string `json:"tenant_id"`
		AutoPost       bool   `json:"auto_post"`
		TokenExpiresAt string `json:"token_expires_at"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !full.Connected || full.TenantID != "tenant-1" || full.AutoPost {
		t.Fatalf("status: %+v", full)
	}
	if strings.Contains(rr.Body.String(), "at-1") {
		t.Fatalf("status leaks access token: %s", rr.Body.String())
	}

	// Mappings: member forbidden, admin works.
	rr = doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/mappings", memberTok,
		map[string]any{"auto_post": true}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member mappings: want 403, got %d", rr.Code)
	}
	rr = doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/mappings", a.Token,
		map[string]any{
			"auto_post":        true,
			"account_mappings": []map[string]any{{"component_key": "payroll.net_pay", "account_code": "476"}},
		}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("mappings: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	conn, err := st.(xero.Store).GetXeroConnection(context.Background(), orgID)
	if err != nil || !conn.AutoPost {
		t.Fatalf("auto_post not persisted: %+v %v", conn, err)
	}

	// Disconnect: member forbidden, admin gets 204 and status flips back.
	rr = doReq(t, srv, http.MethodDelete, xeroBase(orgID), memberTok, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member disconnect: want 403, got %d", rr.Code)
	}
	rr = doReq(t, srv, http.MethodDelete, xeroBase(orgID), a.Token, nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("disconnect: want 204, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, srv, http.MethodGet, xeroBase(orgID)+"/status", a.Token, nil, nil)
	if err := json.Unmarshal(rr.Body.Bytes(), &statusOut); err != nil || statusOut.Connected {
		t.Fatalf("status after disconnect: %s", rr.Body.String())
	}
}

// approvedRunWithLines creates a run, calculates two pay lines, previews,
// and submits it, returning the approved run's ID. The submit fires the
// Xero auto-post hook.
func approvedRunWithLines(t *testing.T, srv *Server, st store.Store, orgID, token string) string {
	t.Helper()
	emp1 := joinedEmployee(t, st, orgID, "XB-1", uniqEmail("xb-1"), store.RoleMember)
	emp2 := joinedEmployee(t, st, orgID, "XB-2", uniqEmail("xb-2"), store.RoleMember)

	rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+orgID+"/payroll-runs", token,
		map[string]any{"period": "2026-10", "country_code": "MY"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create run: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var run struct {
		ID      string `json:"id"`
		Version int64  `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	calc := map[string]any{"employees": []map[string]any{
		{"employee_id": emp1, "base_salary_minor": int64(525075), "age_years": 30, "citizen_my": true},
		{"employee_id": emp2, "base_salary_minor": int64(310050), "age_years": 30, "citizen_my": true},
	}}
	rr = doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+orgID+"/payroll-runs/"+run.ID+"/preview", token, calc, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var pv struct {
		PreviewID string `json:"preview_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &pv); err != nil || pv.PreviewID == "" {
		t.Fatalf("preview id: %v %s", err, rr.Body.String())
	}
	rr = doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+orgID+"/payroll-runs/"+run.ID+"/submit", token,
		map[string]any{"preview_id": pv.PreviewID, "version": run.Version}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("submit: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	return run.ID
}

func TestXeroPostBillRoutes(t *testing.T) {
	fake := &xero.FakeClient{
		ExchangeTokens: xero.Tokens{AccessToken: "at-1", RefreshToken: "rt-1",
			ExpiresAt: time.Now().Add(time.Hour)},
		Tenants:    []xero.Tenant{{TenantID: "tenant-1"}},
		BillResult: xero.BillResult{InvoiceID: "inv-1", InvoiceNumber: "INV-0001", Status: "DRAFT"},
	}
	srv, st, a, _ := newXeroTestServer(t, fake)
	orgID := a.Organization.ID
	runID := approvedRunWithLines(t, srv, st, orgID, a.Token)
	billPath := "/api/v1/organizations/" + orgID + "/payroll-runs/" + runID + "/xero-bill"

	// Unconnected: 404.
	rr := doReq(t, srv, http.MethodPost, billPath, a.Token, map[string]any{"dry_run": true}, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unconnected: want 404, got %d: %s", rr.Code, rr.Body.String())
	}

	connectViaAPI(t, srv, fake, orgID, a.Token)

	// Dry run: 200 with the would-be bills, no Xero call.
	rr = doReq(t, srv, http.MethodPost, billPath, a.Token, map[string]any{"dry_run": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("dry run: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var dry struct {
		DryRun bool `json:"dry_run"`
		Bills  []struct {
			LineItems []struct {
				UnitAmount float64 `json:"UnitAmount"`
			} `json:"LineItems"`
		} `json:"bills"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &dry); err != nil || !dry.DryRun || len(dry.Bills) != 2 {
		t.Fatalf("dry run body: %v %s", err, rr.Body.String())
	}
	if fake.BillCallCount() != 0 {
		t.Fatalf("dry run called xero %d times", fake.BillCallCount())
	}

	// Live post: 201, one bill per line.
	rr = doReq(t, srv, http.MethodPost, billPath, a.Token, map[string]any{"dry_run": false},
		map[string]string{"Idempotency-Key": "bill-key-1"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("post: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if fake.BillCallCount() != 2 {
		t.Fatalf("xero calls: want 2, got %d", fake.BillCallCount())
	}
	// Idempotent replay: the middleware returns the cached 201, Xero is not
	// called again.
	rr2 := doReq(t, srv, http.MethodPost, billPath, a.Token, map[string]any{"dry_run": false},
		map[string]string{"Idempotency-Key": "bill-key-1"})
	if rr2.Code != http.StatusCreated || rr2.Body.String() != rr.Body.String() {
		t.Fatalf("replay: want cached 201, got %d", rr2.Code)
	}
	if fake.BillCallCount() != 2 {
		t.Fatalf("replay called xero: %d calls", fake.BillCallCount())
	}
	// A fresh key re-runs the handler but the ledger guard skips the
	// already-posted employees: still no new Xero calls.
	rr = doReq(t, srv, http.MethodPost, billPath, a.Token, map[string]any{"dry_run": false},
		map[string]string{"Idempotency-Key": "bill-key-2"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("re-post: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if fake.BillCallCount() != 2 {
		t.Fatalf("re-post called xero: %d calls", fake.BillCallCount())
	}
}

func TestXeroAutoPostOnSubmit(t *testing.T) {
	fake := &xero.FakeClient{
		ExchangeTokens: xero.Tokens{AccessToken: "at-1", RefreshToken: "rt-1",
			ExpiresAt: time.Now().Add(time.Hour)},
		Tenants:    []xero.Tenant{{TenantID: "tenant-1"}},
		BillResult: xero.BillResult{InvoiceID: "inv-auto", InvoiceNumber: "INV-AUTO", Status: "DRAFT"},
	}
	srv, st, a, _ := newXeroTestServer(t, fake)
	orgID := a.Organization.ID

	connectViaAPI(t, srv, fake, orgID, a.Token)
	// Enable auto_post via the mappings route.
	rr := doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/mappings", a.Token,
		map[string]any{"auto_post": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("mappings: want 200, got %d: %s", rr.Code, rr.Body.String())
	}

	runID := approvedRunWithLines(t, srv, st, orgID, a.Token)
	if fake.BillCallCount() != 2 {
		t.Fatalf("auto-post calls: want 2, got %d", fake.BillCallCount())
	}
	postings, err := st.(xero.Store).ListBillPostings(context.Background(), orgID, runID)
	if err != nil || len(postings) != 2 {
		t.Fatalf("postings: %+v %v", postings, err)
	}
	for _, p := range postings {
		if p.Status != store.XeroBillPostingPosted || p.XeroInvoiceNumber != "INV-AUTO" {
			t.Fatalf("posting: %+v", p)
		}
	}
}

func TestXeroAutoPostNeverFailsSubmit(t *testing.T) {
	fake := &xero.FakeClient{
		ExchangeTokens: xero.Tokens{AccessToken: "at-1", RefreshToken: "rt-1",
			ExpiresAt: time.Now().Add(time.Hour)},
		Tenants: []xero.Tenant{{TenantID: "tenant-1"}},
		BillErr: &xero.XeroError{StatusCode: 500, Message: "xero is down"},
	}
	srv, st, a, _ := newXeroTestServer(t, fake)
	orgID := a.Organization.ID

	connectViaAPI(t, srv, fake, orgID, a.Token)
	rr := doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/mappings", a.Token,
		map[string]any{"auto_post": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("mappings: %d", rr.Code)
	}

	// The submit still succeeds even though every Xero post fails.
	runID := approvedRunWithLines(t, srv, st, orgID, a.Token)
	postings, err := st.(xero.Store).ListBillPostings(context.Background(), orgID, runID)
	if err != nil || len(postings) != 2 {
		t.Fatalf("postings: %+v %v", postings, err)
	}
	for _, p := range postings {
		if p.Status != store.XeroBillPostingFailed {
			t.Fatalf("posting: %+v", p)
		}
	}
}

func TestXeroBillFailedWebhook(t *testing.T) {
	fake := &xero.FakeClient{
		ExchangeTokens: xero.Tokens{AccessToken: "at-1", RefreshToken: "rt-1",
			ExpiresAt: time.Now().Add(time.Hour)},
		Tenants: []xero.Tenant{{TenantID: "tenant-1"}},
		BillErr: &xero.XeroError{StatusCode: 500, Message: "xero is down"},
	}
	srv, st, a, _ := newXeroTestServer(t, fake)
	orgID := a.Organization.ID
	ctx := context.Background()

	// Subscribe to xero.bill_failed before the submit.
	if _, err := st.CreateWebhookSubscription(ctx, store.WebhookSubscription{
		OrganizationID: orgID, URL: "https://hooks.example.test/xero",
		Secret: "s3cret", Events: []string{store.WebhookEventXeroBillFailed}, Active: true,
	}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	connectViaAPI(t, srv, fake, orgID, a.Token)
	rr := doReq(t, srv, http.MethodPost, xeroBase(orgID)+"/mappings", a.Token,
		map[string]any{"auto_post": true}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("mappings: %d", rr.Code)
	}

	approvedRunWithLines(t, srv, st, orgID, a.Token)

	msgs, err := st.ClaimWebhookDeliveries(ctx, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	failed := 0
	for _, m := range msgs {
		if m.EventType == store.WebhookEventXeroBillFailed {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("xero.bill_failed events: want 2, got %d (%+v)", failed, msgs)
	}
}
