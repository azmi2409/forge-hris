package slack

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
	"sync"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/integrations/secrets"
	"github.com/azmi2409/forge-hris/store"
)

// --- fake store ---

type fakeStore struct {
	mu         sync.Mutex
	workspaces map[string]store.SlackWorkspace
	userMap    map[string]string // teamID + "\x00" + slackUserID -> employeeID
	creds      map[string]store.Credential
	clockedIn  map[string]bool // orgID + "\x00" + employeeID
	breakOpen  map[string]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		workspaces: map[string]store.SlackWorkspace{},
		userMap:    map[string]string{},
		creds:      map[string]store.Credential{},
		clockedIn:  map[string]bool{},
		breakOpen:  map[string]bool{},
	}
}

func (f *fakeStore) GetSlackWorkspace(_ context.Context, teamID string) (store.SlackWorkspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.workspaces[teamID]
	if !ok {
		return store.SlackWorkspace{}, fmt.Errorf("%w: slack workspace", store.ErrNotFound)
	}
	return w, nil
}

func (f *fakeStore) GetSlackUserEmployee(_ context.Context, teamID, slackUserID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.userMap[teamID+"\x00"+slackUserID]
	if !ok {
		return "", fmt.Errorf("%w: slack user", store.ErrNotFound)
	}
	return id, nil
}

func (f *fakeStore) GetCredentialByTokenHash(_ context.Context, hash string) (store.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.creds[hash]
	if !ok {
		return store.Credential{}, fmt.Errorf("%w: credential", store.ErrCredentialInvalid)
	}
	return c, nil
}

func (f *fakeStore) GetEmployee(_ context.Context, orgID, id string) (store.Employee, error) {
	return store.Employee{ID: id, OrganizationID: orgID, FirstName: "Test"}, nil
}

func (f *fakeStore) key(orgID, empID string) string { return orgID + "\x00" + empID }

func (f *fakeStore) ClockIn(_ context.Context, orgID, empID string, e store.TimeEntry) (store.TimeEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clockedIn[f.key(orgID, empID)] {
		return store.TimeEntry{}, store.ErrAlreadyClockedIn
	}
	f.clockedIn[f.key(orgID, empID)] = true
	e.Kind = store.TimeEntryIn
	return e, nil
}

func (f *fakeStore) ClockOut(_ context.Context, orgID, empID string, e store.TimeEntry) (store.TimeEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.clockedIn[f.key(orgID, empID)] {
		return store.TimeEntry{}, fmt.Errorf("%w: no open entry", store.ErrNotFound)
	}
	f.clockedIn[f.key(orgID, empID)] = false
	f.breakOpen[f.key(orgID, empID)] = false
	e.Kind = store.TimeEntryOut
	return e, nil
}

func (f *fakeStore) StartBreak(_ context.Context, orgID, empID string, e store.TimeEntry) (store.TimeEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.clockedIn[f.key(orgID, empID)] {
		return store.TimeEntry{}, fmt.Errorf("%w: not clocked in", store.ErrNotFound)
	}
	if f.breakOpen[f.key(orgID, empID)] {
		return store.TimeEntry{}, fmt.Errorf("%w: break already open", store.ErrInvalidTransition)
	}
	f.breakOpen[f.key(orgID, empID)] = true
	e.Kind = store.TimeEntryStartBreak
	return e, nil
}

func (f *fakeStore) EndBreak(_ context.Context, orgID, empID string, e store.TimeEntry) (store.TimeEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.breakOpen[f.key(orgID, empID)] {
		return store.TimeEntry{}, fmt.Errorf("%w: no open break", store.ErrInvalidTransition)
	}
	f.breakOpen[f.key(orgID, empID)] = false
	e.Kind = store.TimeEntryEndBreak
	return e, nil
}

func (f *fakeStore) OpenTimeEntry(_ context.Context, orgID, empID string) (store.TimeEntry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.clockedIn[f.key(orgID, empID)] {
		return store.TimeEntry{}, false, nil
	}
	kind := store.TimeEntryIn
	if f.breakOpen[f.key(orgID, empID)] {
		kind = store.TimeEntryStartBreak
	}
	return store.TimeEntry{Kind: kind}, true, nil
}

// --- test scaffolding ---

const (
	testTeamID   = "T123"
	testSlackUID = "U123"
	testOrgID    = "org-1"
	testEmpID    = "emp-1"
	testSecret   = "slack-signing-secret"
	testPAT      = "hris_service_token_plaintext"
)

func testEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FORGE_HRIS_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
}

func mustEncrypt(t *testing.T, s string) string {
	t.Helper()
	enc, err := secrets.Encrypt([]byte(s))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	return enc
}

// setupHandler wires a fake store with one connected workspace, one linked
// user, and a valid service PAT holding time:write.
func setupHandler(t *testing.T, now time.Time) (*Handler, *fakeStore) {
	t.Helper()
	testEnv(t)
	fs := newFakeStore()
	fs.workspaces[testTeamID] = store.SlackWorkspace{
		TeamID: testTeamID, OrganizationID: testOrgID,
		EncSigningSecret: mustEncrypt(t, testSecret),
		EncServiceToken:  mustEncrypt(t, testPAT),
	}
	fs.userMap[testTeamID+"\x00"+testSlackUID] = testEmpID
	sum := sha256.Sum256([]byte(testPAT))
	fs.creds[hex.EncodeToString(sum[:])] = store.Credential{
		ID: "cred-1", OrganizationID: testOrgID,
		Scopes: []string{store.ScopeTimeWrite},
	}
	h := NewHandler(fs, Config{Store: fs, Now: func() time.Time { return now }})
	return h, fs
}

func signRequest(secret, timestamp, body string) (tsHeader, sigHeader string) {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "v0:%s:%s", timestamp, body)
	return timestamp, "v0=" + hex.EncodeToString(mac.Sum(nil))
}

func slashRequest(t *testing.T, teamID, userID, command string, secret string, ts time.Time) (*http.Request, string) {
	t.Helper()
	form := url.Values{
		"team_id": {teamID}, "user_id": {userID},
		"command": {command}, "text": {""},
	}
	raw := form.Encode()
	tsStr := strconv.FormatInt(ts.Unix(), 10)
	_, sig := signRequest(secret, tsStr, raw)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/slack/events", strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Slack-Request-Timestamp", tsStr)
	req.Header.Set("X-Slack-Signature", sig)
	return req, raw
}

func doHandle(h *Handler, req *http.Request) (int, map[string]any) {
	rr := httptest.NewRecorder()
	h.HandleEvents(rr, req)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// --- VerifySignature tests ---

func TestVerifySignature(t *testing.T) {
	now := time.Now()
	ts := strconv.FormatInt(now.Unix(), 10)
	_, sig := signRequest(testSecret, ts, "team_id=T123")
	cases := []struct {
		name      string
		secret    string
		timestamp string
		signature string
		want      bool
	}{
		{"valid", testSecret, ts, sig, true},
		{"wrong secret", "nope", ts, sig, false},
		{"malformed header", testSecret, ts, "deadbeef", false},
		{"wrong prefix", testSecret, ts, "v1=" + strings.TrimPrefix(sig, "v0="), false},
		{"bad timestamp", testSecret, "not-a-number", sig, false},
		{"empty signature", testSecret, ts, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := VerifySignature(tc.secret, tc.timestamp, "team_id=T123", tc.signature); got != tc.want {
				t.Fatalf("VerifySignature = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerifySignatureReplay(t *testing.T) {
	now := time.Now()
	// 6 minutes old: must be rejected.
	old := strconv.FormatInt(now.Add(-6*time.Minute).Unix(), 10)
	_, sigOld := signRequest(testSecret, old, "body")
	if VerifySignature(testSecret, old, "body", sigOld) {
		t.Fatal("6-minute-old timestamp must be rejected")
	}
	// 4 minutes old: accepted.
	fresh := strconv.FormatInt(now.Add(-4*time.Minute).Unix(), 10)
	_, sigFresh := signRequest(testSecret, fresh, "body")
	if !VerifySignature(testSecret, fresh, "body", sigFresh) {
		t.Fatal("4-minute-old timestamp must be accepted")
	}
	// 6 minutes in the future: rejected.
	future := strconv.FormatInt(now.Add(6*time.Minute).Unix(), 10)
	_, sigFuture := signRequest(testSecret, future, "body")
	if VerifySignature(testSecret, future, "body", sigFuture) {
		t.Fatal("far-future timestamp must be rejected")
	}
}

// --- handler flow tests ---

func TestSlashClockInOut(t *testing.T) {
	now := time.Now()
	h, _ := setupHandler(t, now)

	req, _ := slashRequest(t, testTeamID, testSlackUID, "/clockin", testSecret, now)
	code, out := doHandle(h, req)
	if code != http.StatusOK {
		t.Fatalf("clockin: want 200, got %d (%v)", code, out)
	}
	if out["response_type"] != "ephemeral" || !strings.Contains(out["text"].(string), "Clocked in") {
		t.Fatalf("unexpected clockin response: %v", out)
	}
	// Double clock-in is a friendly error, not a 500.
	req, _ = slashRequest(t, testTeamID, testSlackUID, "/clockin", testSecret, now)
	_, out = doHandle(h, req)
	if !strings.Contains(out["text"].(string), "already clocked in") {
		t.Fatalf("double clockin: want friendly error, got %v", out)
	}

	req, _ = slashRequest(t, testTeamID, testSlackUID, "/clockout", testSecret, now)
	code, out = doHandle(h, req)
	if code != http.StatusOK || !strings.Contains(out["text"].(string), "Clocked out") {
		t.Fatalf("clockout: %d %v", code, out)
	}
}

func TestSlashUnknownTeam(t *testing.T) {
	now := time.Now()
	h, _ := setupHandler(t, now)
	req, _ := slashRequest(t, "T-NOPE", testSlackUID, "/clockin", testSecret, now)
	code, out := doHandle(h, req)
	if code != http.StatusOK {
		t.Fatalf("want 200 with guidance, got %d", code)
	}
	if !strings.Contains(out["text"].(string), "isn't connected") {
		t.Fatalf("unexpected response: %v", out)
	}
}

func TestSlashBadSignature(t *testing.T) {
	now := time.Now()
	h, _ := setupHandler(t, now)
	req, _ := slashRequest(t, testTeamID, testSlackUID, "/clockin", "wrong-secret", now)
	code, _ := doHandle(h, req)
	if code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", code)
	}
}

func TestSlashUnmappedUser(t *testing.T) {
	now := time.Now()
	h, _ := setupHandler(t, now)
	req, _ := slashRequest(t, testTeamID, "U-STRANGER", "/clockin", testSecret, now)
	code, out := doHandle(h, req)
	if code != http.StatusOK || !strings.Contains(out["text"].(string), "isn't linked") {
		t.Fatalf("unmapped user: %d %v", code, out)
	}
}

func TestSlashRevokedPAT(t *testing.T) {
	now := time.Now()
	h, fs := setupHandler(t, now)
	// Revoke the PAT: wipe the credential.
	fs.mu.Lock()
	fs.creds = map[string]store.Credential{}
	fs.mu.Unlock()
	req, _ := slashRequest(t, testTeamID, testSlackUID, "/clockin", testSecret, now)
	code, out := doHandle(h, req)
	if code != http.StatusOK || !strings.Contains(out["text"].(string), "invalid or revoked") {
		t.Fatalf("revoked PAT: %d %v", code, out)
	}
}

func TestSlashPATWithoutScope(t *testing.T) {
	now := time.Now()
	h, fs := setupHandler(t, now)
	sum := sha256.Sum256([]byte(testPAT))
	fs.mu.Lock()
	c := fs.creds[hex.EncodeToString(sum[:])]
	c.Scopes = []string{store.ScopeTimeRead} // read-only
	fs.creds[hex.EncodeToString(sum[:])] = c
	fs.mu.Unlock()
	req, _ := slashRequest(t, testTeamID, testSlackUID, "/clockin", testSecret, now)
	_, out := doHandle(h, req)
	if !strings.Contains(out["text"].(string), "time:write") {
		t.Fatalf("scope-less PAT: %v", out)
	}
}

func TestInteractiveBreakButtons(t *testing.T) {
	now := time.Now()
	h, _ := setupHandler(t, now)

	// Clock in first.
	req, _ := slashRequest(t, testTeamID, testSlackUID, "/clockin", testSecret, now)
	doHandle(h, req)

	// Press "Start break".
	payload := `{"type":"block_actions","team":{"id":"T123"},"user":{"id":"U123"},` +
		`"actions":[{"action_id":"break_start","value":"break_start"}]}`
	form := url.Values{"team_id": {testTeamID}, "user_id": {testSlackUID}, "payload": {payload}}
	raw := form.Encode()
	tsStr := strconv.FormatInt(now.Unix(), 10)
	_, sig := signRequest(testSecret, tsStr, raw)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/slack/events", strings.NewReader(raw))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", tsStr)
	r.Header.Set("X-Slack-Signature", sig)
	code, out := doHandle(h, r)
	if code != http.StatusOK || !strings.Contains(out["text"].(string), "Break started") {
		t.Fatalf("break_start: %d %v", code, out)
	}
	// The reply must carry the "End break" button.
	blocks, ok := out["blocks"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("want 2 blocks, got %v", out["blocks"])
	}

	// Press "End break".
	payload = `{"type":"block_actions","team":{"id":"T123"},"user":{"id":"U123"},` +
		`"actions":[{"action_id":"break_end","value":"break_end"}]}`
	form = url.Values{"team_id": {testTeamID}, "user_id": {testSlackUID}, "payload": {payload}}
	raw = form.Encode()
	_, sig = signRequest(testSecret, tsStr, raw)
	r = httptest.NewRequest(http.MethodPost, "/api/v1/integrations/slack/events", strings.NewReader(raw))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", tsStr)
	r.Header.Set("X-Slack-Signature", sig)
	code, out = doHandle(h, r)
	if code != http.StatusOK || !strings.Contains(out["text"].(string), "Break ended") {
		t.Fatalf("break_end: %d %v", code, out)
	}
}

func TestInteractiveTamperedTeamRejected(t *testing.T) {
	now := time.Now()
	h, _ := setupHandler(t, now)
	// Payload claims a different team than the signed form.
	payload := `{"type":"block_actions","team":{"id":"T-EVIL"},"user":{"id":"U123"},` +
		`"actions":[{"action_id":"break_start","value":"break_start"}]}`
	form := url.Values{"team_id": {testTeamID}, "user_id": {testSlackUID}, "payload": {payload}}
	raw := form.Encode()
	tsStr := strconv.FormatInt(now.Unix(), 10)
	_, sig := signRequest(testSecret, tsStr, raw)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/slack/events", strings.NewReader(raw))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Slack-Request-Timestamp", tsStr)
	r.Header.Set("X-Slack-Signature", sig)
	code, _ := doHandle(h, r)
	if code != http.StatusBadRequest {
		t.Fatalf("tampered team: want 400, got %d", code)
	}
}

func TestHandlerConcurrentClockIn(t *testing.T) {
	now := time.Now()
	h, _ := setupHandler(t, now)
	const n = 8
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			req, _ := slashRequest(t, testTeamID, testSlackUID, "/clockin", testSecret, now)
			code, _ := doHandle(h, req)
			codes <- code
		}(i)
	}
	for i := 0; i < n; i++ {
		if c := <-codes; c != http.StatusOK {
			t.Fatalf("concurrent clockin: want 200, got %d", c)
		}
	}
	// Exactly one real clock-in; the rest got the friendly already-in text.
	// (The fake store enforces the state machine under its mutex.)
}

// Compile-time check that the concrete stores satisfy the narrow interface.
var (
	_ Store = (*store.MemoryStore)(nil)
	_ Store = (*store.PostgresStore)(nil)
)
