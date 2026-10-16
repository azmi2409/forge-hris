package xero

// Handler tests: OAuth connect/callback, status, mappings, disconnect,
// bill posting (dry-run and live), and the auto-post hook — all over the
// MemoryStore and the FakeClient. No live Xero anywhere.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/integrations/secrets"
	"github.com/azmi2409/forge-hris/store"
)

func testSecretKey(t *testing.T) {
	t.Helper()
	t.Setenv("FORGE_HRIS_SECRET_KEY", "0123456789abcdef0123456789abcdef")
}

var orgSeq atomic.Int64

func testOrg(t *testing.T, st *store.MemoryStore) string {
	t.Helper()
	org, err := st.CreateOrganization(context.Background(), store.Organization{
		Name:        "Xero Handler Co",
		Slug:        fmt.Sprintf("xero-h-%d", orgSeq.Add(1)),
		CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org.ID
}

// testRun creates a draft payroll run with two pay lines.
func testRun(t *testing.T, st *store.MemoryStore, orgID, period, country string) (string, []string) {
	t.Helper()
	ctx := context.Background()
	run, err := st.CreatePayrollRun(ctx, store.PayrollRun{
		OrganizationID: orgID, Period: period, CountryCode: country,
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	var empIDs []string
	for i, net := range []int64{525075, 310050} { // sen: 5250.75, 3100.50
		emp, err := st.CreateEmployee(ctx, store.Employee{
			OrganizationID: orgID, EmployeeNumber: "E-" + period + "-" + string(rune('A'+i)),
			FirstName: "Emp", LastName: string(rune('A' + i)), Status: store.InvitationJoined,
		})
		if err != nil {
			t.Fatalf("CreateEmployee: %v", err)
		}
		if _, err := st.UpsertPayLine(ctx, store.EmployeePayLine{
			OrganizationID: orgID, RunID: run.ID, EmployeeID: emp.ID,
			NetMinor: net, GrossMinor: net,
		}); err != nil {
			t.Fatalf("UpsertPayLine: %v", err)
		}
		empIDs = append(empIDs, emp.ID)
	}
	// Bills only post for approved runs: move the run through the gate the
	// same way the submit endpoint does.
	if _, err := st.TransitionPayrollRun(ctx, orgID, run.ID, run.Version, store.PayrollRunApproved, "test"); err != nil {
		t.Fatalf("TransitionPayrollRun: %v", err)
	}
	return run.ID, empIDs
}

func newTestHandler(st Store, fake *FakeClient) *Handler {
	return NewHandler(st, Config{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
		RedirectURL:  "https://app.example.test/api/v1/integrations/xero/callback",
		Client:       fake,
	})
}

func decodeJSONBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v: %s", err, rr.Body.String())
	}
	return out
}

// connectFlow runs HandleConnect and returns the issued state.
func connectFlow(t *testing.T, h *Handler, orgID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/connect", nil)
	rr := httptest.NewRecorder()
	h.HandleConnect(rr, req, orgID, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("connect: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	out := decodeJSONBody(t, rr)
	authURL, _ := out["authorization_url"].(string)
	state, _ := out["state"].(string)
	if state == "" {
		t.Fatalf("connect: missing state: %s", rr.Body.String())
	}
	for _, want := range []string{
		"https://login.xero.com/identity/connect/authorize",
		"response_type=code", "client_id=test-client-id",
		"scope=", "state=" + state,
	} {
		if !strings.Contains(authURL, want) {
			t.Fatalf("authorization_url missing %q:\n%s", want, authURL)
		}
	}
	return state
}

func TestHandleConnect(t *testing.T) {
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	h := newTestHandler(st, &FakeClient{})

	state := connectFlow(t, h, orgID)
	if state == "" {
		t.Fatal("empty state")
	}

	// Non-admin is rejected.
	req := httptest.NewRequest(http.MethodPost, "/connect", nil)
	rr := httptest.NewRecorder()
	h.HandleConnect(rr, req, orgID, false)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", rr.Code)
	}

	// Unconfigured handler (no client ID) is a 503, not a redirect to a
	// broken URL.
	h2 := NewHandler(st, Config{Client: &FakeClient{}})
	req = httptest.NewRequest(http.MethodPost, "/connect", nil)
	rr = httptest.NewRecorder()
	h2.HandleConnect(rr, req, orgID, true)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured: want 503, got %d: %s", rr.Code, rr.Body.String())
	}
	if out := decodeJSONBody(t, rr); out["code"] != "XERO_NOT_CONFIGURED" {
		t.Fatalf("code: %v", out)
	}
}

func TestHandleCallbackFullFlow(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	fake := &FakeClient{
		ExchangeTokens: Tokens{
			AccessToken: "at-123", RefreshToken: "rt-456",
			ExpiresAt: time.Now().Add(time.Hour),
		},
		Tenants: []Tenant{{TenantID: "tenant-aaa"}},
	}
	h := newTestHandler(st, fake)
	state := connectFlow(t, h, orgID)

	req := httptest.NewRequest(http.MethodGet,
		"/callback?code=auth-code-1&state="+state, nil)
	rr := httptest.NewRecorder()
	h.HandleCallback(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("callback: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	out := decodeJSONBody(t, rr)
	if out["connected"] != true || out["tenant_id"] != "tenant-aaa" {
		t.Fatalf("callback response: %v", out)
	}

	// The stored tokens are sealed envelopes, never plaintext.
	conn, err := st.GetXeroConnection(context.Background(), orgID)
	if err != nil {
		t.Fatalf("GetXeroConnection: %v", err)
	}
	if strings.Contains(conn.EncAccessToken, "at-123") || conn.EncAccessToken == "" {
		t.Fatalf("access token stored in plaintext or empty")
	}
	dec, err := secrets.Decrypt(conn.EncAccessToken)
	if err != nil || string(dec) != "at-123" {
		t.Fatalf("decrypt access: %q %v", dec, err)
	}

	// State is single-use: replaying it fails.
	req = httptest.NewRequest(http.MethodGet,
		"/callback?code=auth-code-1&state="+state, nil)
	rr = httptest.NewRecorder()
	h.HandleCallback(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("replay: want 400, got %d", rr.Code)
	}
	if out := decodeJSONBody(t, rr); out["code"] != "XERO_INVALID_STATE" {
		t.Fatalf("replay code: %v", out)
	}

	// Unknown state fails.
	req = httptest.NewRequest(http.MethodGet, "/callback?code=x&state=bogus", nil)
	rr = httptest.NewRecorder()
	h.HandleCallback(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bogus state: want 400, got %d", rr.Code)
	}

	// Xero denying the grant surfaces XERO_OAUTH_DENIED.
	req = httptest.NewRequest(http.MethodGet, "/callback?error=access_denied", nil)
	rr = httptest.NewRecorder()
	h.HandleCallback(rr, req)
	if rr.Code != http.StatusBadRequest || decodeJSONBody(t, rr)["code"] != "XERO_OAUTH_DENIED" {
		t.Fatalf("denied: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHandleCallbackExchangeFailure(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	fake := &FakeClient{ExchangeErr: &XeroError{StatusCode: 400, Type: "invalid_grant"}}
	h := newTestHandler(st, fake)
	state := connectFlow(t, h, orgID)

	req := httptest.NewRequest(http.MethodGet, "/callback?code=bad&state="+state, nil)
	rr := httptest.NewRecorder()
	h.HandleCallback(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rr.Code, rr.Body.String())
	}
	if out := decodeJSONBody(t, rr); out["code"] != "XERO_TOKEN_EXCHANGE_FAILED" {
		t.Fatalf("code: %v", out)
	}
}

func TestHandleStatus(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	h := newTestHandler(st, &FakeClient{})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	rr := httptest.NewRecorder()
	h.HandleStatus(rr, req, orgID)
	if out := decodeJSONBody(t, rr); out["connected"] != false {
		t.Fatalf("unconnected: %v", out)
	}

	if _, err := st.SaveXeroConnection(context.Background(), store.XeroConnection{
		OrganizationID: orgID, TenantID: "t-1",
		EncAccessToken: "enc", EncRefreshToken: "enc",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	rr = httptest.NewRecorder()
	h.HandleStatus(rr, req, orgID)
	out := decodeJSONBody(t, rr)
	if out["connected"] != true || out["tenant_id"] != "t-1" {
		t.Fatalf("connected: %v", out)
	}
	for _, leaked := range []string{"access_token", "refresh_token", "enc_access_token"} {
		if _, ok := out[leaked]; ok {
			t.Fatalf("status leaks %s", leaked)
		}
	}
}

// connectTestOrg stores a connected org with real sealed token envelopes
// (callers must set the secrets key first). autoPost controls the flag.
func connectTestOrg(t *testing.T, st *store.MemoryStore, orgID string, autoPost bool) {
	t.Helper()
	encAccess, err := secrets.Encrypt([]byte("tok-access-" + orgID))
	if err != nil {
		t.Fatalf("seal access: %v", err)
	}
	encRefresh, err := secrets.Encrypt([]byte("tok-refresh-" + orgID))
	if err != nil {
		t.Fatalf("seal refresh: %v", err)
	}
	if _, err := st.SaveXeroConnection(context.Background(), store.XeroConnection{
		OrganizationID: orgID, TenantID: "t-1",
		EncAccessToken: encAccess, EncRefreshToken: encRefresh,
		ExpiresAt: time.Now().Add(time.Hour), AutoPost: autoPost,
	}); err != nil {
		t.Fatalf("connect org: %v", err)
	}
}

func TestHandleMappings(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	h := newTestHandler(st, &FakeClient{})

	body := `{"auto_post": true,
		"account_mappings": [{"component_key": "payroll.net_pay", "account_code": "476"}],
		"contact_mappings": [{"employee_id": "emp-1", "contact_id": "contact-1"}]}`

	// Mappings require a connection first.
	req := httptest.NewRequest(http.MethodPost, "/mappings", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.HandleMappings(rr, req, orgID, true)
	if rr.Code != http.StatusNotFound || decodeJSONBody(t, rr)["code"] != "XERO_NOT_CONNECTED" {
		t.Fatalf("no connection: %d %s", rr.Code, rr.Body.String())
	}

	connectTestOrg(t, st, orgID, false)
	req = httptest.NewRequest(http.MethodPost, "/mappings", strings.NewReader(body))
	rr = httptest.NewRecorder()
	h.HandleMappings(rr, req, orgID, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("mappings: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	out := decodeJSONBody(t, rr)
	if out["auto_post"] != true {
		t.Fatalf("auto_post: %v", out)
	}
	accts, conts, err := st.GetXeroMappings(context.Background(), orgID)
	if err != nil || len(accts) != 1 || accts[0].AccountCode != "476" ||
		len(conts) != 1 || conts[0].ContactID != "contact-1" {
		t.Fatalf("stored mappings: %+v %+v %v", accts, conts, err)
	}
	conn, _ := st.GetXeroConnection(context.Background(), orgID)
	if !conn.AutoPost {
		t.Fatalf("auto_post not persisted")
	}

	// Non-admin is rejected.
	req = httptest.NewRequest(http.MethodPost, "/mappings", strings.NewReader(body))
	rr = httptest.NewRecorder()
	h.HandleMappings(rr, req, orgID, false)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", rr.Code)
	}

	// Validation: empty account_code.
	req = httptest.NewRequest(http.MethodPost, "/mappings",
		strings.NewReader(`{"account_mappings": [{"component_key": "x", "account_code": ""}]}`))
	rr = httptest.NewRecorder()
	h.HandleMappings(rr, req, orgID, true)
	if rr.Code != http.StatusBadRequest || decodeJSONBody(t, rr)["code"] != "VALIDATION_ERROR" {
		t.Fatalf("bad mapping: %d %s", rr.Code, rr.Body.String())
	}
}

func TestHandleDisconnect(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	h := newTestHandler(st, &FakeClient{})

	req := httptest.NewRequest(http.MethodDelete, "/xero", nil)
	rr := httptest.NewRecorder()
	h.HandleDisconnect(rr, req, orgID, false)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("non-admin: want 403, got %d", rr.Code)
	}

	connectTestOrg(t, st, orgID, false)
	req = httptest.NewRequest(http.MethodDelete, "/xero", nil)
	rr = httptest.NewRecorder()
	h.HandleDisconnect(rr, req, orgID, true)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("disconnect: want 204, got %d", rr.Code)
	}
	if _, err := st.GetXeroConnection(context.Background(), orgID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("connection survives: %v", err)
	}
	// Idempotent: disconnecting again is still 204.
	req = httptest.NewRequest(http.MethodDelete, "/xero", nil)
	rr = httptest.NewRecorder()
	h.HandleDisconnect(rr, req, orgID, true)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("second disconnect: want 204, got %d", rr.Code)
	}
}

func TestBuildBills(t *testing.T) {
	run := store.PayrollRun{ID: "run-1", OrganizationID: "org-1", Period: "2026-10", CountryCode: "MY"}
	lines := []store.EmployeePayLine{
		{EmployeeID: "emp-1", NetMinor: 525075}, // 5250.75 MYR
		{EmployeeID: "emp-2", NetMinor: 310050},
	}
	accounts := []store.XeroAccountMapping{{ComponentKey: "payroll.net_pay", AccountCode: "999"}}
	contacts := []store.XeroContactMapping{{EmployeeID: "emp-1", ContactID: "contact-1"}}

	bills := buildBills(run, lines, accounts, contacts)
	if len(bills) != 2 {
		t.Fatalf("bills: %d", len(bills))
	}
	b := bills[0]
	if b.Type != "ACCPAY" || b.Contact.ContactID != "contact-1" {
		t.Fatalf("bill 0: %+v", b)
	}
	if len(b.LineItems) != 1 || b.LineItems[0].UnitAmount != 5250.75 ||
		b.LineItems[0].AccountCode != "999" || b.LineItems[0].TaxType != "NONE" {
		t.Fatalf("line 0: %+v", b.LineItems)
	}
	if b.Reference != "PAYROLL 2026-10 emp-1" {
		t.Fatalf("reference: %q", b.Reference)
	}
	// Unmapped employee falls back to a named contact and the mapped account.
	if bills[1].Contact.Name != "Employee emp-2" || bills[1].Contact.ContactID != "" {
		t.Fatalf("fallback contact: %+v", bills[1].Contact)
	}

	// No account mapping: default 476.
	bills = buildBills(run, lines, nil, nil)
	if bills[0].LineItems[0].AccountCode != DefaultNetPayAccount {
		t.Fatalf("default account: %+v", bills[0].LineItems[0])
	}

	// IDR: whole rupiah, no division.
	run.CountryCode = "ID"
	bills = buildBills(run, []store.EmployeePayLine{{EmployeeID: "e", NetMinor: 5250750}}, nil, nil)
	if bills[0].LineItems[0].UnitAmount != 5250750 {
		t.Fatalf("IDR amount: %v", bills[0].LineItems[0].UnitAmount)
	}
}

func TestHandlePostBillDryRun(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	runID, empIDs := testRun(t, st, orgID, "2026-10", "MY")
	fake := &FakeClient{}
	h := newTestHandler(st, fake)

	// Dry run without a connection is still a 404.
	req := httptest.NewRequest(http.MethodPost, "/xero-bill", nil)
	rr := httptest.NewRecorder()
	h.HandlePostBill(rr, req, orgID, runID, true, true)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("dry run unconnected: want 404, got %d", rr.Code)
	}

	connectTestOrg(t, st, orgID, false)
	if err := st.SaveXeroMappings(context.Background(), orgID,
		[]store.XeroAccountMapping{{ComponentKey: "payroll.net_pay", AccountCode: "476"}},
		[]store.XeroContactMapping{{EmployeeID: empIDs[0], ContactID: "contact-1"}}); err != nil {
		t.Fatalf("mappings: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/xero-bill", nil)
	rr = httptest.NewRecorder()
	h.HandlePostBill(rr, req, orgID, runID, true, true)
	if rr.Code != http.StatusOK {
		t.Fatalf("dry run: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		DryRun bool   `json:"dry_run"`
		Bills  []Bill `json:"bills"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.DryRun || len(out.Bills) != 2 {
		t.Fatalf("dry run: %+v", out)
	}
	if out.Bills[0].Contact.ContactID != "contact-1" {
		t.Fatalf("dry bill contact: %+v", out.Bills[0].Contact)
	}
	if out.Bills[0].LineItems[0].UnitAmount != 5250.75 {
		t.Fatalf("dry bill amount: %+v", out.Bills[0].LineItems[0])
	}
	if fake.BillCallCount() != 0 {
		t.Fatalf("dry run called Xero %d times", fake.BillCallCount())
	}
	// Dry run records nothing.
	if rows, _ := st.ListBillPostings(context.Background(), orgID, runID); len(rows) != 0 {
		t.Fatalf("dry run recorded postings: %+v", rows)
	}
}

func TestHandlePostBillDraftRunRejected(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	ctx := context.Background()
	orgID := testOrg(t, st)
	// A draft run: bills must never post for it.
	run, err := st.CreatePayrollRun(ctx, store.PayrollRun{
		OrganizationID: orgID, Period: "2026-11", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	h := newTestHandler(st, &FakeClient{})
	connectTestOrg(t, st, orgID, false)

	req := httptest.NewRequest(http.MethodPost, "/xero-bill", nil)
	rr := httptest.NewRecorder()
	h.HandlePostBill(rr, req, orgID, run.ID, true, true) // even dry_run is refused
	if rr.Code != http.StatusConflict {
		t.Fatalf("draft run: want 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if out := decodeJSONBody(t, rr); out["code"] != "XERO_RUN_NOT_APPROVED" {
		t.Fatalf("code: %v", out)
	}
}

func TestHandlePostBillLive(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	runID, _ := testRun(t, st, orgID, "2026-10", "MY")
	fake := &FakeClient{BillResult: BillResult{
		InvoiceID: "inv-1", InvoiceNumber: "INV-0001", Status: "DRAFT",
	}}
	h := newTestHandler(st, fake)
	connectTestOrg(t, st, orgID, false)

	req := httptest.NewRequest(http.MethodPost, "/xero-bill", nil)
	rr := httptest.NewRecorder()
	h.HandlePostBill(rr, req, orgID, runID, true, false)
	if rr.Code != http.StatusCreated {
		t.Fatalf("post: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if fake.BillCallCount() != 2 {
		t.Fatalf("xero calls: want 2, got %d", fake.BillCallCount())
	}
	rows, err := st.ListBillPostings(context.Background(), orgID, runID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("postings: %+v %v", rows, err)
	}
	for _, p := range rows {
		if p.Status != store.XeroBillPostingPosted || p.XeroInvoiceNumber != "INV-0001" {
			t.Fatalf("posting: %+v", p)
		}
	}

	// Posting again is idempotent: already-posted employees are skipped.
	req = httptest.NewRequest(http.MethodPost, "/xero-bill", nil)
	rr = httptest.NewRecorder()
	h.HandlePostBill(rr, req, orgID, runID, true, false)
	if rr.Code != http.StatusCreated {
		t.Fatalf("re-post: want 201, got %d", rr.Code)
	}
	if fake.BillCallCount() != 2 {
		t.Fatalf("re-post called xero again: %d calls", fake.BillCallCount())
	}
}

func TestHandlePostBillXeroFailure(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	runID, _ := testRun(t, st, orgID, "2026-10", "MY")
	fake := &FakeClient{BillErr: &XeroError{StatusCode: 400, Message: "bad account code"}}
	h := newTestHandler(st, fake)
	connectTestOrg(t, st, orgID, false)

	req := httptest.NewRequest(http.MethodPost, "/xero-bill", nil)
	rr := httptest.NewRecorder()
	h.HandlePostBill(rr, req, orgID, runID, true, false)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rr.Code, rr.Body.String())
	}
	out := decodeJSONBody(t, rr)
	if out["code"] != "XERO_API_ERROR" {
		t.Fatalf("code: %v", out)
	}
	if postings, ok := out["postings"].([]any); !ok || len(postings) != 2 {
		t.Fatalf("postings in 502 body: %v", out)
	}
	// Failures are recorded in the ledger so the next attempt can retry
	// (failed rows are not skipped by the idempotency guard).
	rows, err := st.ListBillPostings(context.Background(), orgID, runID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("postings: %+v %v", rows, err)
	}
	for _, p := range rows {
		if p.Status != store.XeroBillPostingFailed || !strings.Contains(p.Error, "bad account code") {
			t.Fatalf("failed posting: %+v", p)
		}
	}
}

func TestHandlePostBillRefreshesExpiredToken(t *testing.T) {
	testSecretKey(t)
	st := store.NewMemoryStore()
	orgID := testOrg(t, st)
	runID, _ := testRun(t, st, orgID, "2026-10", "MY")

	encAccess, _ := secrets.Encrypt([]byte("stale-access"))
	encRefresh, _ := secrets.Encrypt([]byte("old-refresh"))
	if _, err := st.SaveXeroConnection(context.Background(), store.XeroConnection{
		OrganizationID: orgID, TenantID: "t-1",
		EncAccessToken: encAccess, EncRefreshToken: encRefresh,
		ExpiresAt: time.Now().Add(-time.Hour), // expired
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	fake := &FakeClient{
		RefreshTokens_: Tokens{AccessToken: "fresh-access", RefreshToken: "fresh-refresh",
			ExpiresAt: time.Now().Add(time.Hour)},
		BillResult: BillResult{InvoiceID: "inv-1", Status: "DRAFT"},
	}
	h := newTestHandler(st, fake)

	req := httptest.NewRequest(http.MethodPost, "/xero-bill", nil)
	rr := httptest.NewRecorder()
	h.HandlePostBill(rr, req, orgID, runID, true, false)
	if rr.Code != http.StatusCreated {
		t.Fatalf("post: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(fake.RefreshCalls) != 1 || fake.RefreshCalls[0] != "old-refresh" {
		t.Fatalf("refresh calls: %+v", fake.RefreshCalls)
	}
	// The refreshed token was used for the bill and re-sealed in storage.
	if fake.BillCalls[0].AccessToken != "fresh-access" {
		t.Fatalf("bill auth: %q", fake.BillCalls[0].AccessToken)
	}
	conn, _ := st.GetXeroConnection(context.Background(), orgID)
	dec, err := secrets.Decrypt(conn.EncAccessToken)
	if err != nil || string(dec) != "fresh-access" {
		t.Fatalf("stored token not refreshed: %q %v", dec, err)
	}
}

func TestMaybeAutoPostBill(t *testing.T) {
	testSecretKey(t)
	ctx := context.Background()

	t.Run("no connection is a no-op", func(t *testing.T) {
		st := store.NewMemoryStore()
		orgID := testOrg(t, st)
		runID, _ := testRun(t, st, orgID, "2026-10", "MY")
		fake := &FakeClient{}
		if err := MaybeAutoPostBill(ctx, st, fake, orgID, runID, nil); err != nil {
			t.Fatalf("no-op: %v", err)
		}
		if fake.BillCallCount() != 0 {
			t.Fatalf("called xero without a connection")
		}
	})

	t.Run("auto_post disabled is a no-op", func(t *testing.T) {
		st := store.NewMemoryStore()
		orgID := testOrg(t, st)
		runID, _ := testRun(t, st, orgID, "2026-10", "MY")
		connectTestOrg(t, st, orgID, false)
		fake := &FakeClient{}
		if err := MaybeAutoPostBill(ctx, st, fake, orgID, runID, nil); err != nil {
			t.Fatalf("no-op: %v", err)
		}
		if fake.BillCallCount() != 0 {
			t.Fatalf("called xero with auto_post disabled")
		}
	})

	t.Run("posts when enabled", func(t *testing.T) {
		st := store.NewMemoryStore()
		orgID := testOrg(t, st)
		runID, _ := testRun(t, st, orgID, "2026-10", "MY")
		connectTestOrg(t, st, orgID, true)
		fake := &FakeClient{BillResult: BillResult{InvoiceID: "inv-9", InvoiceNumber: "INV-9"}}
		if err := MaybeAutoPostBill(ctx, st, fake, orgID, runID, nil); err != nil {
			t.Fatalf("auto-post: %v", err)
		}
		if fake.BillCallCount() != 2 {
			t.Fatalf("calls: %d", fake.BillCallCount())
		}
		rows, _ := st.ListBillPostings(ctx, orgID, runID)
		if len(rows) != 2 || rows[0].XeroInvoiceNumber != "INV-9" {
			t.Fatalf("postings: %+v", rows)
		}
	})

	t.Run("xero error never fails the approval", func(t *testing.T) {
		st := store.NewMemoryStore()
		orgID := testOrg(t, st)
		runID, _ := testRun(t, st, orgID, "2026-10", "MY")
		connectTestOrg(t, st, orgID, true)
		fake := &FakeClient{BillErr: &XeroError{StatusCode: 500, Message: "xero is down"}}
		if err := MaybeAutoPostBill(ctx, st, fake, orgID, runID, nil); err != nil {
			t.Fatalf("must return nil on xero failure, got %v", err)
		}
		rows, _ := st.ListBillPostings(ctx, orgID, runID)
		if len(rows) != 2 {
			t.Fatalf("failed postings not recorded: %+v", rows)
		}
		for _, p := range rows {
			if p.Status != store.XeroBillPostingFailed {
				t.Fatalf("posting: %+v", p)
			}
		}
	})
}

func TestStateStoreExpiry(t *testing.T) {
	ss := newStateStore()
	state, err := ss.issue("org-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if orgID, err := ss.consume(state); err != nil || orgID != "org-1" {
		t.Fatalf("consume: %q %v", orgID, err)
	}
	if _, err := ss.consume(state); err == nil {
		t.Fatal("double consume: want error")
	}
	// Expired entries are evicted on next use.
	state2, _ := ss.issue("org-2")
	ss.mu.Lock()
	e := ss.entries[state2]
	e.expiresAt = time.Now().Add(-time.Second)
	ss.entries[state2] = e
	ss.mu.Unlock()
	if _, err := ss.consume(state2); err == nil {
		t.Fatal("expired state: want error")
	}
}
