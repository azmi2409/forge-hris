package xero

// Client tests: the real HTTP client against httptest servers serving the
// hand-written fixtures in testdata/. No live Xero anywhere.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestExchangeCodeSuccess(t *testing.T) {
	var gotForm, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connect/token" {
			t.Errorf("path: got %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = r.ParseForm()
		gotForm = r.Form.Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "token_exchange_success.json"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient("cid", "csecret", srv.URL, srv.URL)
	toks, err := c.ExchangeCode(context.Background(), "auth-code-123", "https://app.example.test/xero/callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if toks.AccessToken != "xero-access-token-abc123" || toks.RefreshToken != "xero-refresh-token-def456" {
		t.Fatalf("tokens: %+v", toks)
	}
	if time.Until(toks.ExpiresAt) < 29*time.Minute {
		t.Fatalf("expires_at too soon: %v", toks.ExpiresAt)
	}
	// Form encoding + Basic auth per Xero's token endpoint contract.
	if !strings.Contains(gotForm, "grant_type=authorization_code") ||
		!strings.Contains(gotForm, "code=auth-code-123") ||
		!strings.Contains(gotForm, "redirect_uri=") {
		t.Fatalf("form: %s", gotForm)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("auth header: %q", gotAuth)
	}
}

func TestExchangeCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(fixture(t, "token_exchange_error.json"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient("cid", "csecret", srv.URL, srv.URL)
	_, err := c.ExchangeCode(context.Background(), "stale-code", "https://app.example.test/xero/callback")
	var xe *XeroError
	if !errors.As(err, &xe) {
		t.Fatalf("want *XeroError, got %T (%v)", err, err)
	}
	if xe.StatusCode != http.StatusBadRequest || xe.Type != "invalid_grant" {
		t.Fatalf("typed error: %+v", xe)
	}
	if !strings.Contains(xe.Error(), "400") {
		t.Fatalf("Error(): %q", xe.Error())
	}
}

func TestRefreshTokens(t *testing.T) {
	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form.Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "token_refresh_success.json"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient("cid", "csecret", srv.URL, srv.URL)
	toks, err := c.RefreshTokens(context.Background(), "old-refresh-token")
	if err != nil {
		t.Fatalf("RefreshTokens: %v", err)
	}
	if toks.AccessToken != "xero-access-token-refreshed-789" ||
		toks.RefreshToken != "xero-refresh-token-rotated-012" {
		t.Fatalf("tokens: %+v", toks)
	}
	if !strings.Contains(gotForm, "grant_type=refresh_token") ||
		!strings.Contains(gotForm, "refresh_token=old-refresh-token") {
		t.Fatalf("form: %s", gotForm)
	}
}

func TestCreateBillSuccess(t *testing.T) {
	var gotHeaders http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api.xro/2.0/Invoices" {
			t.Errorf("path: got %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method: got %s", r.Method)
		}
		gotHeaders = r.Header
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "invoice_create_success.json"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient("cid", "csecret", srv.URL, srv.URL)
	res, err := c.CreateBill(context.Background(), "tok", "tenant-1", Bill{
		Type:      "ACCPAY",
		Contact:   BillContact{ContactID: "c9e8d7c6-b5a4-4f3e-2d1c-0b9a8f7e6d5c4"},
		Date:      "2026-10-31",
		DueDate:   "2026-11-07",
		Reference: "PAYROLL 2026-10 emp-1",
		LineItems: []BillLineItem{{
			Description: "Net pay 2026-10", Quantity: 1, UnitAmount: 5250.75,
			AccountCode: "476", TaxType: "NONE",
		}},
	})
	if err != nil {
		t.Fatalf("CreateBill: %v", err)
	}
	if res.InvoiceID != "8d1d3e2a-4b5c-4d6e-8f90-a1b2c3d4e5f6" ||
		res.InvoiceNumber != "INV-0042" || res.Status != "DRAFT" {
		t.Fatalf("result: %+v", res)
	}
	if gotHeaders.Get("Authorization") != "Bearer tok" {
		t.Fatalf("authorization: %q", gotHeaders.Get("Authorization"))
	}
	if gotHeaders.Get("Xero-Tenant-Id") != "tenant-1" {
		t.Fatalf("tenant: %q", gotHeaders.Get("Xero-Tenant-Id"))
	}
	if gotHeaders.Get("Accept") != "application/json" {
		t.Fatalf("accept: %q", gotHeaders.Get("Accept"))
	}
	body := string(gotBody)
	for _, want := range []string{
		`"Type":"ACCPAY"`, `"ContactID":"c9e8d7c6-b5a4-4f3e-2d1c-0b9a8f7e6d5c4"`,
		`"Reference":"PAYROLL 2026-10 emp-1"`, `"AccountCode":"476"`,
		`"UnitAmount":5250.75`, `"TaxType":"NONE"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("request body missing %s:\n%s", want, body)
		}
	}
}

func TestCreateBillValidationError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(fixture(t, "invoice_create_validation_error.json"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient("cid", "csecret", srv.URL, srv.URL)
	_, err := c.CreateBill(context.Background(), "tok", "tenant-1", Bill{Contact: BillContact{}})
	var xe *XeroError
	if !errors.As(err, &xe) {
		t.Fatalf("want *XeroError, got %T (%v)", err, err)
	}
	if xe.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: %+v", xe)
	}
	if xe.Message != "A validation exception occurred" {
		t.Fatalf("message: %+v", xe)
	}
	if len(xe.Details) != 2 ||
		xe.Details[0] != "The contact name must be specified." ||
		!strings.Contains(xe.Details[1], "Account code") {
		t.Fatalf("details: %+v", xe)
	}
}

func TestListTenants(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/connections" {
			t.Errorf("path: got %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "connections_success.json"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient("cid", "csecret", srv.URL, srv.URL)
	tenants, err := c.ListTenants(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(tenants) != 1 || tenants[0].TenantID != "tenant-id-aaa-111" ||
		tenants[0].TenantName != "Test Org MY" {
		t.Fatalf("tenants: %+v", tenants)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth: %q", gotAuth)
	}
}

func TestBillDefaultsACCPAY(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t, "invoice_create_success.json"))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient("cid", "csecret", srv.URL, srv.URL)
	if _, err := c.CreateBill(context.Background(), "tok", "t", Bill{}); err != nil {
		t.Fatalf("empty bill still posts as ACCPAY: %v", err)
	}
}
