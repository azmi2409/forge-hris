package api

// Webhook API tests: admin-only enforcement on every webhook endpoint, the
// secret-once contract, and the end-to-end path — create a subscription,
// trigger employee.created through the API, run one dispatcher pass, and
// verify the receiver got a correctly-signed envelope with a unique id.
//
// The store underneath is the MemoryStore. webhookRoutes is registered
// explicitly here because Server.routes() is owned by the coordinator's
// integration step.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/azmi2409/forge-hris/store"
)

type webhookReceiver struct {
	mu       sync.Mutex
	requests []receivedWebhook
}

type receivedWebhook struct {
	body      []byte
	signature string
	event     string
	id        string
}

func newWebhookReceiver(t *testing.T) (*webhookReceiver, *httptest.Server) {
	t.Helper()
	rec := &webhookReceiver{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		rec.mu.Lock()
		rec.requests = append(rec.requests, receivedWebhook{
			body:      body,
			signature: r.Header.Get("X-Hub-Signature-256"),
			event:     r.Header.Get("X-Webhook-Event"),
			id:        r.Header.Get("X-Webhook-ID"),
		})
		rec.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *webhookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *webhookReceiver) first() receivedWebhook {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[0]
}

func webhookTestServer(t *testing.T) (*Server, store.Store, bootstrapOut) {
	t.Helper()
	srv, st := newTestServer(t)
	// Webhook routes are registered by Server.routes().
	b := bootstrap(t, srv, "webhooks")
	return srv, st, b
}

func createWebhookSub(t *testing.T, srv *Server, orgID, token, url string, events []string) (string, string) {
	t.Helper()
	rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+orgID+"/webhooks", token, map[string]any{
		"url": url, "events": events,
	}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create subscription: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		ID     string `json:"id"`
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode subscription: %v", err)
	}
	if out.ID == "" || out.Secret == "" {
		t.Fatalf("create must reveal id and secret once: %+v", out)
	}
	return out.ID, out.Secret
}

func TestWebhookAdminOnly(t *testing.T) {
	srv, st, b := webhookTestServer(t)
	memberID := joinedEmployee(t, st, b.Organization.ID, "E-mem", uniqEmail("wmem"), store.RoleMember)
	memberToken := "tok-wmem-plain"
	mintToken(t, st, b.Organization.ID, "pat", "member token", memberToken, &memberID, nil)
	base := "/api/v1/organizations/" + b.Organization.ID + "/webhooks"

	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, base, map[string]any{"url": "https://x.test/h", "events": []string{"*"}}},
		{http.MethodGet, base, nil},
		{http.MethodGet, base + "/nope", nil},
		{http.MethodPatch, base + "/nope", map[string]any{"active": false}},
		{http.MethodDelete, base + "/nope", nil},
		{http.MethodPost, base + "/nope/test", nil},
	} {
		rr := doReq(t, srv, tc.method, tc.path, memberToken, tc.body, nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s %s: member want 403, got %d: %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
		rr = doReq(t, srv, tc.method, tc.path, "", tc.body, nil)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: anonymous want 401, got %d", tc.method, tc.path, rr.Code)
		}
	}
	// Cross-tenant: the other org's admin can't touch this org's webhooks.
	other := bootstrap(t, srv, "webhooks-other")
	rr := doReq(t, srv, http.MethodGet, base, other.Token, nil, nil)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-org list: want 403, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestWebhookSecretOnceAndRedaction(t *testing.T) {
	srv, _, b := webhookTestServer(t)
	base := "/api/v1/organizations/" + b.Organization.ID + "/webhooks"

	subID, secret := createWebhookSub(t, srv, b.Organization.ID, b.Token, "https://hooks.example.test/x", []string{"*"})

	// Get and list must NOT reveal the secret.
	rr := doReq(t, srv, http.MethodGet, base+"/"+subID, b.Token, nil, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if got.Secret != "" {
		t.Fatalf("get revealed the secret")
	}
	rr = doReq(t, srv, http.MethodGet, base, b.Token, nil, nil)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("list leaked the secret: %d %s", rr.Code, rr.Body.String())
	}

	// Bad input is rejected with a validation error.
	rr = doReq(t, srv, http.MethodPost, base, b.Token, map[string]any{
		"url": "not-a-url", "events": []string{"*"},
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad url: want 400, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, srv, http.MethodPost, base, b.Token, map[string]any{
		"url": "https://x.test/h", "events": []string{"bogus.event"},
	}, nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("bad event: want 400, got %d: %s", rr.Code, rr.Body.String())
	}

	// PATCH with a new secret rotates it and echoes the new one.
	rr = doReq(t, srv, http.MethodPatch, base+"/"+subID, b.Token, map[string]any{
		"secret": "rotated-secret",
	}, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var patched struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patched.Secret != "rotated-secret" {
		t.Fatalf("rotation not echoed: %+v", patched)
	}
}

func TestWebhookEndToEndDelivery(t *testing.T) {
	srv, _, b := webhookTestServer(t)
	rec, rsrv := newWebhookReceiver(t)

	_, secret := createWebhookSub(t, srv, b.Organization.ID, b.Token, rsrv.URL,
		[]string{store.WebhookEventEmployeeCreated})

	// A second subscription with non-matching events must NOT fire.
	_, _ = createWebhookSub(t, srv, b.Organization.ID, b.Token, rsrv.URL,
		[]string{store.WebhookEventLeaveRequested})

	// Trigger employee.created through the API.
	rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+b.Organization.ID+"/employees", b.Token,
		map[string]any{
			"first_name": "Web", "last_name": "Hook", "email": uniqEmail("e2e"),
		}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create employee: want 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var emp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &emp); err != nil {
		t.Fatalf("decode employee: %v", err)
	}

	// One dispatcher pass delivers to the matching subscription only.
	dispatchWebhookPass(context.Background(), srv.store, srv.logger)
	if rec.count() != 1 {
		t.Fatalf("receiver got %d requests, want 1", rec.count())
	}
	rw := rec.first()

	// Signature: sha256=<hex(HMAC-SHA256(secret, body))>.
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rw.body)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if rw.signature != wantSig {
		t.Fatalf("signature mismatch:\n got %q\nwant %q", rw.signature, wantSig)
	}
	if rw.event != store.WebhookEventEmployeeCreated {
		t.Fatalf("X-Webhook-Event = %q", rw.event)
	}

	// Envelope: unique id, type, time, and the employee in data.
	var env struct {
		ID   string          `json:"id"`
		Type string          `json:"type"`
		Time string          `json:"time"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rw.body, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.ID == "" || rw.id != env.ID {
		t.Fatalf("envelope id missing or header mismatch: %+v header=%q", env, rw.id)
	}
	if env.Type != store.WebhookEventEmployeeCreated || env.Time == "" {
		t.Fatalf("bad envelope: %+v", env)
	}
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil || data.ID != emp.ID {
		t.Fatalf("envelope data wrong employee: %s err=%v", env.Data, err)
	}

	// The message was marked sent: another pass finds nothing new.
	dispatchWebhookPass(context.Background(), srv.store, srv.logger)
	if rec.count() != 1 {
		t.Fatalf("after second pass: %d requests, want 1", rec.count())
	}
	// (Exactly one request also proves the non-matching subscription did
	// not fire.)
}

func TestWebhookTestEndpointFlow(t *testing.T) {
	srv, _, b := webhookTestServer(t)
	rec, rsrv := newWebhookReceiver(t)
	base := "/api/v1/organizations/" + b.Organization.ID + "/webhooks"

	subID, secret := createWebhookSub(t, srv, b.Organization.ID, b.Token, rsrv.URL,
		[]string{store.WebhookEventEmployeeCreated})

	// POST /test enqueues webhook.test through the normal pipeline and
	// returns the outbox message id.
	rr := doReq(t, srv, http.MethodPost, base+"/"+subID+"/test", b.Token, nil, nil)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("test endpoint: want 202, got %d: %s", rr.Code, rr.Body.String())
	}
	var out struct {
		MessageID string `json:"message_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil || out.MessageID == "" {
		t.Fatalf("test response: %s err=%v", rr.Body.String(), err)
	}

	dispatchWebhookPass(context.Background(), srv.store, srv.logger)
	if rec.count() != 1 {
		t.Fatalf("receiver got %d requests, want 1", rec.count())
	}
	rw := rec.first()
	if rw.event != store.WebhookEventWebhookTest {
		t.Fatalf("X-Webhook-Event = %q, want webhook.test", rw.event)
	}
	if rw.id != out.MessageID {
		t.Fatalf("X-Webhook-ID = %q, want message id %q", rw.id, out.MessageID)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rw.body)
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); rw.signature != want {
		t.Fatalf("test delivery signature mismatch")
	}

	// Deleting the subscription drops everything; the id is gone.
	rr = doReq(t, srv, http.MethodDelete, base+"/"+subID, b.Token, nil, nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: want 204, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = doReq(t, srv, http.MethodGet, base+"/"+subID, b.Token, nil, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("get after delete: want 404, got %d", rr.Code)
	}
}

func TestWebhookDeliveryFailureBacksOff(t *testing.T) {
	srv, _, b := webhookTestServer(t)
	// Receiver that always answers 500.
	var mu sync.Mutex
	got := 0
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	_, _ = createWebhookSub(t, srv, b.Organization.ID, b.Token, bad.URL,
		[]string{store.WebhookEventEmployeeCreated})

	rr := doReq(t, srv, http.MethodPost, "/api/v1/organizations/"+b.Organization.ID+"/employees", b.Token,
		map[string]any{"first_name": "Fail", "last_name": "Case", "email": uniqEmail("fail")}, nil)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create employee: want 201, got %d", rr.Code)
	}

	// First pass: one attempt, message stays pending (not sent).
	dispatchWebhookPass(context.Background(), srv.store, srv.logger)
	mu.Lock()
	n := got
	mu.Unlock()
	if n != 1 {
		t.Fatalf("endpoint hit %d times after first pass, want 1", n)
	}

	// Second pass immediately: the failed message is on backoff (30s), so
	// there is nothing due and the endpoint is not hammered.
	dispatchWebhookPass(context.Background(), srv.store, srv.logger)
	mu.Lock()
	n = got
	mu.Unlock()
	if n != 1 {
		t.Fatalf("endpoint hit %d times after second pass, want still 1 (backoff)", n)
	}
	// The dead-after-5-attempts transition itself is covered at the store
	// level (TestWebhookClaimMarkFlows).
}
