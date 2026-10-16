package teams

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/azmi2409/forge-hris/integrations/secrets"
	"github.com/azmi2409/forge-hris/store"
)

// --- fake store ---

type fakeStore struct {
	mu        sync.Mutex
	configs   map[string]store.TeamsConfig // orgID
	userMap   map[string]string            // orgID + "\x00" + teamsUserID -> employeeID
	clockedIn map[string]bool
	breakOpen map[string]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		configs:   map[string]store.TeamsConfig{},
		userMap:   map[string]string{},
		clockedIn: map[string]bool{},
		breakOpen: map[string]bool{},
	}
}

func (f *fakeStore) ListTeamsConfigs(_ context.Context, _ store.Page) ([]store.TeamsConfig, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.TeamsConfig
	for _, c := range f.configs {
		out = append(out, c)
	}
	return out, "", nil
}

func (f *fakeStore) GetTeamsUserEmployee(_ context.Context, orgID, teamsUserID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.userMap[orgID+"\x00"+teamsUserID]
	if !ok {
		return "", fmt.Errorf("%w: teams user", store.ErrNotFound)
	}
	return id, nil
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

// --- scaffolding ---

const (
	testOrgID   = "org-1"
	testTeamsID = "teams-user-1"
	testEmpID   = "emp-1"
	testKey     = "teams-shared-secret"
)

func setupHandler(t *testing.T) (*Handler, *fakeStore) {
	t.Helper()
	t.Setenv("FORGE_HRIS_SECRET_KEY", base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	enc, err := secrets.Encrypt([]byte(testKey))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	fs := newFakeStore()
	fs.configs[testOrgID] = store.TeamsConfig{
		OrganizationID: testOrgID, TenantRef: "tenant-1", EncSharedSecret: enc,
	}
	fs.userMap[testOrgID+"\x00"+testTeamsID] = testEmpID
	return NewHandler(fs, Config{Store: fs}), fs
}

func webhookRequest(key, userID, text string) *http.Request {
	body, _ := json.Marshal(map[string]string{"text": text, "user_id": userID})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/teams/webhook?key="+key,
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func doHandle(h *Handler, req *http.Request) (int, map[string]string) {
	rr := httptest.NewRecorder()
	h.HandleWebhook(rr, req)
	var out map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// --- tests ---

func TestClockInOut(t *testing.T) {
	h, _ := setupHandler(t)
	code, out := doHandle(h, webhookRequest(testKey, testTeamsID, "clock in"))
	if code != http.StatusOK || !strings.Contains(out["text"], "Clocked in") {
		t.Fatalf("clock in: %d %v", code, out)
	}
	// Case-insensitive prefix: "CLOCK OUT now please" still clocks out.
	code, out = doHandle(h, webhookRequest(testKey, testTeamsID, "CLOCK OUT now please"))
	if code != http.StatusOK || !strings.Contains(out["text"], "Clocked out") {
		t.Fatalf("clock out: %d %v", code, out)
	}
}

func TestDoubleClockIn(t *testing.T) {
	h, _ := setupHandler(t)
	doHandle(h, webhookRequest(testKey, testTeamsID, "clock in"))
	code, out := doHandle(h, webhookRequest(testKey, testTeamsID, "clock in"))
	if code != http.StatusOK || !strings.Contains(out["text"], "already clocked in") {
		t.Fatalf("double clock in: %d %v", code, out)
	}
}

func TestBreakToggle(t *testing.T) {
	h, _ := setupHandler(t)
	doHandle(h, webhookRequest(testKey, testTeamsID, "clock in"))
	// "break" starts a break when none is open.
	_, out := doHandle(h, webhookRequest(testKey, testTeamsID, "break"))
	if !strings.Contains(out["text"], "Break started") {
		t.Fatalf("break start: %v", out)
	}
	// "break" again ends it.
	_, out = doHandle(h, webhookRequest(testKey, testTeamsID, "break"))
	if !strings.Contains(out["text"], "Break ended") {
		t.Fatalf("break end: %v", out)
	}
}

func TestMissingKey(t *testing.T) {
	h, _ := setupHandler(t)
	req := webhookRequest("", testTeamsID, "clock in")
	code, _ := doHandle(h, req)
	if code != http.StatusUnauthorized {
		t.Fatalf("missing key: want 401, got %d", code)
	}
}

func TestBadKey(t *testing.T) {
	h, _ := setupHandler(t)
	code, _ := doHandle(h, webhookRequest("wrong-secret", testTeamsID, "clock in"))
	if code != http.StatusUnauthorized {
		t.Fatalf("bad key: want 401, got %d", code)
	}
}

func TestUnmappedUser(t *testing.T) {
	h, _ := setupHandler(t)
	code, out := doHandle(h, webhookRequest(testKey, "stranger", "clock in"))
	if code != http.StatusOK || !strings.Contains(out["text"], "isn't linked") {
		t.Fatalf("unmapped user: %d %v", code, out)
	}
}

func TestBadJSON(t *testing.T) {
	h, _ := setupHandler(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/integrations/teams/webhook?key="+testKey,
		strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	code, _ := doHandle(h, req)
	if code != http.StatusBadRequest {
		t.Fatalf("bad json: want 400, got %d", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	h, _ := setupHandler(t)
	code, out := doHandle(h, webhookRequest(testKey, testTeamsID, "hello there"))
	if code != http.StatusOK || !strings.Contains(out["text"], "clock in") {
		t.Fatalf("unknown command: %d %v", code, out)
	}
}

func TestWrongMethod(t *testing.T) {
	h, _ := setupHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations/teams/webhook?key="+testKey, nil)
	code, _ := doHandle(h, req)
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: want 405, got %d", code)
	}
}

func TestConcurrentBreakToggle(t *testing.T) {
	h, _ := setupHandler(t)
	doHandle(h, webhookRequest(testKey, testTeamsID, "clock in"))
	const n = 8
	done := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			code, _ := doHandle(h, webhookRequest(testKey, testTeamsID, "break"))
			done <- code
		}()
	}
	for i := 0; i < n; i++ {
		if c := <-done; c != http.StatusOK {
			t.Fatalf("concurrent break: want 200, got %d", c)
		}
	}
}

// Compile-time check that the concrete stores satisfy the narrow interface.
var (
	_ Store = (*store.MemoryStore)(nil)
	_ Store = (*store.PostgresStore)(nil)
)
