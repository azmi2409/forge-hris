package api

// API tests for the component 6 chat routes: admin connect/link endpoints
// (RBAC, validation, secret encryption) and the public webhook exemption
// (no forge-hris bearer auth; the Slack signature is the auth).

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/integrations/secrets"
	"github.com/azmi2409/forge-hris/store"
)

func testSecretKey(t *testing.T) {
	t.Helper()
	t.Setenv("FORGE_HRIS_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
}

// svcToken mints a service-account credential and returns its plaintext.
func svcToken(t *testing.T, st store.Store, orgID, name, plaintext string, scopes []string) {
	t.Helper()
	mintToken(t, st, orgID, "service_account", name, plaintext, nil, scopes)
}

func connectSlack(t *testing.T, srv *Server, token, orgID, teamID, svcPlain string) (int, map[string]any) {
	t.Helper()
	rr := doReq(t, srv, http.MethodPost,
		"/api/v1/organizations/"+orgID+"/integrations/slack", token,
		map[string]any{
			"team_id":        teamID,
			"team_name":      "Acme",
			"signing_secret": "xoxb-signing",
			"service_token":  svcPlain,
		}, nil)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

func TestConnectSlack(t *testing.T) {
	testSecretKey(t)
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	orgPath := "/api/v1/organizations/" + f.orgID
	svcPlain := "svc-plaintext-chat-1"
	svcToken(t, st, f.orgID, "slack bot", svcPlain, []string{store.ScopeTimeWrite})

	// Admin connect: 201.
	code, _ := connectSlack(t, srv, f.tokens["admin"], f.orgID, "T100", svcPlain)
	if code != http.StatusCreated {
		t.Fatalf("connect: want 201, got %d", code)
	}
	// Secrets are encrypted at rest — never the plaintext.
	cs := st.(store.ChatStore)
	ws, err := cs.GetSlackWorkspace(context.Background(), "T100")
	if err != nil {
		t.Fatalf("GetSlackWorkspace: %v", err)
	}
	if strings.Contains(ws.EncSigningSecret, "xoxb-signing") || strings.Contains(ws.EncServiceToken, svcPlain) {
		t.Fatal("secrets must be encrypted at rest")
	}
	dec, err := secrets.Decrypt(ws.EncSigningSecret)
	if err != nil || string(dec) != "xoxb-signing" {
		t.Fatalf("signing secret round trip: %v", err)
	}
	dec, err = secrets.Decrypt(ws.EncServiceToken)
	if err != nil || string(dec) != svcPlain {
		t.Fatalf("service token round trip: %v", err)
	}

	// Member may not connect.
	code, _ = connectSlack(t, srv, f.tokens["member"], f.orgID, "T101", svcPlain)
	if code != http.StatusForbidden {
		t.Fatalf("member connect: want 403, got %d", code)
	}
	// No token: 401.
	rr := doReq(t, srv, http.MethodPost, orgPath+"/integrations/slack", "",
		map[string]any{"team_id": "T102", "signing_secret": "x", "service_token": svcPlain}, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: want 401, got %d", rr.Code)
	}
	// Cross-org token: 403.
	code, _ = connectSlack(t, srv, f.tokens["cross"], f.orgID, "T103", svcPlain)
	if code != http.StatusForbidden {
		t.Fatalf("cross-org: want 403, got %d", code)
	}
	// Unknown service token: 400.
	rr = doReq(t, srv, http.MethodPost, orgPath+"/integrations/slack", f.tokens["admin"],
		map[string]any{"team_id": "T104", "signing_secret": "x", "service_token": "bogus"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bogus service token: want 400, got %d", rr.Code)
	}
	// Service token without time:write: 400.
	svcToken(t, st, f.orgID, "read-only", "svc-readonly-1", []string{store.ScopeTimeRead})
	rr = doReq(t, srv, http.MethodPost, orgPath+"/integrations/slack", f.tokens["admin"],
		map[string]any{"team_id": "T105", "signing_secret": "x", "service_token": "svc-readonly-1"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("scope-less token: want 400, got %d", rr.Code)
	}
	// Service token from another org: 400. (Covered in TestConnectSlackForeignToken.)
}

func TestConnectSlackForeignToken(t *testing.T) {
	testSecretKey(t)
	srv, st := newTestServer(t)
	a := bootstrap(t, srv, "chat-a")
	b := bootstrap(t, srv, "chat-b")
	svcToken(t, st, b.Organization.ID, "b svc", "svc-b-plaintext", []string{store.ScopeTimeWrite})
	// A's admin tries to connect with B's service token.
	rr := doReq(t, srv, http.MethodPost,
		"/api/v1/organizations/"+a.Organization.ID+"/integrations/slack", a.Token,
		map[string]any{"team_id": "T200", "signing_secret": "x", "service_token": "svc-b-plaintext"}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("foreign-org token: want 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestLinkSlackUser(t *testing.T) {
	testSecretKey(t)
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	orgPath := "/api/v1/organizations/" + f.orgID
	svcPlain := "svc-plaintext-chat-2"
	svcToken(t, st, f.orgID, "slack bot", svcPlain, []string{store.ScopeTimeWrite})
	if code, _ := connectSlack(t, srv, f.tokens["admin"], f.orgID, "T300", svcPlain); code != http.StatusCreated {
		t.Fatalf("connect: want 201, got %d", code)
	}
	empID := joinedEmployee(t, st, f.orgID, "E-SLK", uniqEmail("slk"), store.RoleMember)

	// Admin link: 201.
	rr := doReq(t, srv, http.MethodPost, orgPath+"/integrations/slack/users", f.tokens["admin"],
		map[string]any{"team_id": "T300", "slack_user_id": "U300", "employee_id": empID}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("link: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	cs := st.(store.ChatStore)
	got, err := cs.GetSlackUserEmployee(context.Background(), "T300", "U300")
	if err != nil || got != empID {
		t.Fatalf("mapping: got %q (%v), want %q", got, err, empID)
	}
	// Member may not link.
	rr = doReq(t, srv, http.MethodPost, orgPath+"/integrations/slack/users", f.tokens["member"],
		map[string]any{"team_id": "T300", "slack_user_id": "U301", "employee_id": empID}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member link: want 403, got %d", rr.Code)
	}
	// Unknown employee: 404.
	rr = doReq(t, srv, http.MethodPost, orgPath+"/integrations/slack/users", f.tokens["admin"],
		map[string]any{"team_id": "T300", "slack_user_id": "U302", "employee_id": "nope"}, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown employee: want 404, got %d", rr.Code)
	}
	// Workspace of another org: 403.
	other := bootstrap(t, srv, "chat-other")
	svcToken(t, st, other.Organization.ID, "other svc", "svc-other-1", []string{store.ScopeTimeWrite})
	rr = doReq(t, srv, http.MethodPost,
		"/api/v1/organizations/"+other.Organization.ID+"/integrations/slack", other.Token,
		map[string]any{"team_id": "T399", "signing_secret": "x", "service_token": "svc-other-1"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("other org connect: want 201, got %d", rr.Code)
	}
	rr = doReq(t, srv, http.MethodPost, orgPath+"/integrations/slack/users", f.tokens["admin"],
		map[string]any{"team_id": "T399", "slack_user_id": "U399", "employee_id": empID}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("foreign workspace: want 403, got %d", rr.Code)
	}
}

func TestConnectAndLinkTeams(t *testing.T) {
	testSecretKey(t)
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	orgPath := "/api/v1/organizations/" + f.orgID

	rr := doReq(t, srv, http.MethodPost, orgPath+"/integrations/teams", f.tokens["admin"],
		map[string]any{"tenant_ref": "tenant-1", "shared_secret": "shh"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("teams connect: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	cs := st.(store.ChatStore)
	cfg, err := cs.GetTeamsConfig(context.Background(), f.orgID)
	if err != nil {
		t.Fatalf("GetTeamsConfig: %v", err)
	}
	if strings.Contains(cfg.EncSharedSecret, "shh") {
		t.Fatal("shared secret must be encrypted at rest")
	}
	dec, err := secrets.Decrypt(cfg.EncSharedSecret)
	if err != nil || string(dec) != "shh" {
		t.Fatalf("shared secret round trip: %v", err)
	}

	empID := joinedEmployee(t, st, f.orgID, "E-TMS", uniqEmail("tms"), store.RoleMember)
	rr = doReq(t, srv, http.MethodPost, orgPath+"/integrations/teams/users", f.tokens["admin"],
		map[string]any{"teams_user_id": "tu-1", "employee_id": empID}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("teams link: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	got, err := cs.GetTeamsUserEmployee(context.Background(), f.orgID, "tu-1")
	if err != nil || got != empID {
		t.Fatalf("mapping: got %q (%v), want %q", got, err, empID)
	}
	// Member may not connect/link.
	rr = doReq(t, srv, http.MethodPost, orgPath+"/integrations/teams", f.tokens["member"],
		map[string]any{"shared_secret": "x"}, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("member teams connect: want 403, got %d", rr.Code)
	}
}

// TestSlackWebhookNoBearerAuth proves the public webhook route bypasses
// forge-hris bearer auth: an unsigned request is rejected by the Slack
// handler (401 bad signature), not by the auth middleware.
func TestSlackWebhookNoBearerAuth(t *testing.T) {
	testSecretKey(t)
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	svcPlain := "svc-plaintext-chat-3"
	svcToken(t, st, f.orgID, "slack bot", svcPlain, []string{store.ScopeTimeWrite})
	if code, _ := connectSlack(t, srv, f.tokens["admin"], f.orgID, "T400", svcPlain); code != http.StatusCreated {
		t.Fatalf("connect: want 201, got %d", code)
	}

	form := url.Values{"team_id": {"T400"}, "user_id": {"U400"}, "command": {"/clockin"}}
	raw := form.Encode()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte("wrong-secret"))
	fmt.Fprintf(mac, "v0:%s:%s", ts, raw)
	sig := "v0=" + hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/slack/events",
		strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad slack signature: want 401 from the handler, got %d (%s)", rr.Code, rr.Body.String())
	}
}

// TestTeamsWebhookNoBearerAuth is the Teams counterpart: no bearer token,
// wrong ?key= -> 401 from the handler.
func TestTeamsWebhookNoBearerAuth(t *testing.T) {
	testSecretKey(t)
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+f.orgID+"/integrations/teams",
		f.tokens["admin"], map[string]any{"shared_secret": "real-key"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("teams connect: want 201, got %d", rr.Code)
	}
	rr = doReq(t, srv, http.MethodPost, "/api/v1/integrations/teams/webhook?key=wrong", "",
		map[string]any{"text": "clock in", "user_id": "u1"}, nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad teams key: want 401 from the handler, got %d (%s)", rr.Code, rr.Body.String())
	}
}

// TestTeamsWebhookEndToEnd clocks in through the public route with the
// real shared secret and a linked user.
func TestTeamsWebhookEndToEnd(t *testing.T) {
	testSecretKey(t)
	srv, st := newTestServer(t)
	f := newRBACFixture(t, srv, st)
	rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+f.orgID+"/integrations/teams",
		f.tokens["admin"], map[string]any{"shared_secret": "e2e-key"}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("teams connect: want 201, got %d", rr.Code)
	}
	empID := joinedEmployee(t, st, f.orgID, "E-TE2", uniqEmail("te2"), store.RoleMember)
	rr = doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+f.orgID+"/integrations/teams/users",
		f.tokens["admin"], map[string]any{"teams_user_id": "tu-e2e", "employee_id": empID}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("teams link: want 201, got %d", rr.Code)
	}
	rr = doReq(t, srv, http.MethodPost, "/api/v1/integrations/teams/webhook?key=e2e-key", "",
		map[string]any{"text": "Clock In", "user_id": "tu-e2e"}, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Clocked in") {
		t.Fatalf("teams clock in: want 200 Clocked in, got %d: %s", rr.Code, rr.Body.String())
	}
	open, isOpen, err := st.OpenTimeEntry(context.Background(), f.orgID, empID)
	if err != nil || !isOpen || open.Kind != store.TimeEntryIn {
		t.Fatalf("employee should be clocked in: %+v %v %v", open, isOpen, err)
	}
}
