package store

// Contract tests for the component 6 ChatStore: run the identical scenario
// against MemoryStore and (when TEST_DATABASE_URL is set) PostgresStore,
// so both adapters honor version checks, identity maps, single-use ESS
// tokens, and never leak secrets through serialization.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func chatFixture(t *testing.T, s ChatStore) (ctx context.Context, orgID, empID string) {
	t.Helper()
	ctx = context.Background()
	// Both MemoryStore and PostgresStore implement the core Store for setup.
	full, ok := s.(interface {
		CreateOrganization(ctx context.Context, o Organization) (Organization, error)
		CreateEmployee(ctx context.Context, e Employee) (Employee, error)
	})
	if !ok {
		t.Fatal("chat test store does not implement core Store")
	}
	org, err := full.CreateOrganization(ctx, Organization{
		Name: "Chat Co", Slug: "chat-co-" + strings.ToLower(newID()[:8]), CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	emp, err := full.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID, EmployeeNumber: "CHAT-1",
		FirstName: "Chat", LastName: "User",
		Email:  "chat-" + strings.ToLower(newID()[:8]) + "@example.test",
		Status: InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	return ctx, org.ID, emp.ID
}

func testChatStoreContract(t *testing.T, s ChatStore) {
	t.Helper()
	ctx, orgID, empID := chatFixture(t, s)

	// --- Slack workspace upsert + version conflict ---
	ws, err := s.SaveSlackWorkspace(ctx, SlackWorkspace{
		TeamID: "T123", OrganizationID: orgID, TeamName: "Acme",
		EncSigningSecret: "enc-signing", EncServiceToken: "enc-token",
	})
	if err != nil {
		t.Fatalf("SaveSlackWorkspace: %v", err)
	}
	if ws.Version != 1 {
		t.Fatalf("new workspace version = %d, want 1", ws.Version)
	}
	got, err := s.GetSlackWorkspace(ctx, "T123")
	if err != nil {
		t.Fatalf("GetSlackWorkspace: %v", err)
	}
	if got.OrganizationID != orgID || got.TeamName != "Acme" {
		t.Fatalf("workspace mismatch: %+v", got)
	}
	// Stale version must conflict.
	stale := ws
	stale.TeamName = "Stale"
	stale.Version = 999
	if _, err := s.SaveSlackWorkspace(ctx, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale save: want ErrVersionConflict, got %v", err)
	}
	// Fresh version updates.
	ws.TeamName = "Acme Corp"
	ws2, err := s.SaveSlackWorkspace(ctx, ws)
	if err != nil {
		t.Fatalf("SaveSlackWorkspace update: %v", err)
	}
	if ws2.Version != 2 || ws2.TeamName != "Acme Corp" {
		t.Fatalf("update mismatch: %+v", ws2)
	}
	// Secrets must never serialize.
	var ser struct {
		TeamID           string `json:"team_id"`
		OrganizationID   string `json:"organization_id"`
		TeamName         string `json:"team_name"`
		EncSigningSecret string `json:"-"`
	}
	b, _ := json.Marshal(ws2)
	if err := json.Unmarshal(b, &ser); err != nil {
		t.Fatalf("unmarshal workspace: %v", err)
	}
	if strings.Contains(string(b), "enc-signing") || strings.Contains(string(b), "enc-token") {
		t.Fatalf("secrets leaked through JSON: %s", b)
	}
	// Unknown org / unknown team.
	if _, err := s.GetSlackWorkspace(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown team: want ErrNotFound, got %v", err)
	}
	if _, err := s.SaveSlackWorkspace(ctx, SlackWorkspace{
		TeamID: "T999", OrganizationID: newID(),
		EncSigningSecret: "x", EncServiceToken: "y",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown org: want ErrNotFound, got %v", err)
	}

	// --- Slack user map ---
	if err := s.MapSlackUser(ctx, "T123", "U123", empID); err != nil {
		t.Fatalf("MapSlackUser: %v", err)
	}
	mapped, err := s.GetSlackUserEmployee(ctx, "T123", "U123")
	if err != nil || mapped != empID {
		t.Fatalf("GetSlackUserEmployee: got %q (%v), want %q", mapped, err, empID)
	}
	if _, err := s.GetSlackUserEmployee(ctx, "T123", "U999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unmapped user: want ErrNotFound, got %v", err)
	}
	// Mapping to an employee of another org must fail.
	if err := s.MapSlackUser(ctx, "T123", "U123", newID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign employee: want ErrNotFound, got %v", err)
	}
	if err := s.UnmapSlackUser(ctx, "T123", "U123"); err != nil {
		t.Fatalf("UnmapSlackUser: %v", err)
	}
	if _, err := s.GetSlackUserEmployee(ctx, "T123", "U123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after unmap: want ErrNotFound, got %v", err)
	}
	if err := s.UnmapSlackUser(ctx, "T123", "U123"); err != nil {
		t.Fatalf("double unmap must be idempotent: %v", err)
	}

	// --- Teams config ---
	cfg, err := s.SaveTeamsConfig(ctx, TeamsConfig{
		OrganizationID: orgID, TenantRef: "tenant-1", EncSharedSecret: "enc-shared",
	})
	if err != nil {
		t.Fatalf("SaveTeamsConfig: %v", err)
	}
	if cfg.Version != 1 {
		t.Fatalf("new teams config version = %d, want 1", cfg.Version)
	}
	gotCfg, err := s.GetTeamsConfig(ctx, orgID)
	if err != nil || gotCfg.TenantRef != "tenant-1" {
		t.Fatalf("GetTeamsConfig: %+v (%v)", gotCfg, err)
	}
	if _, err := s.GetTeamsConfig(ctx, newID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown teams org: want ErrNotFound, got %v", err)
	}
	listed, _, err := s.ListTeamsConfigs(ctx, Page{})
	if err != nil || len(listed) != 1 || listed[0].OrganizationID != orgID {
		t.Fatalf("ListTeamsConfigs: %+v (%v)", listed, err)
	}
	// Version conflict on update.
	cfgBad := cfg
	cfgBad.Version = 42
	if _, err := s.SaveTeamsConfig(ctx, cfgBad); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("teams stale save: want ErrVersionConflict, got %v", err)
	}
	if err := s.MapTeamsUser(ctx, orgID, "teams-user-1", empID); err != nil {
		t.Fatalf("MapTeamsUser: %v", err)
	}
	mapped, err = s.GetTeamsUserEmployee(ctx, orgID, "teams-user-1")
	if err != nil || mapped != empID {
		t.Fatalf("GetTeamsUserEmployee: got %q (%v), want %q", mapped, err, empID)
	}
	if err := s.DeleteTeamsConfig(ctx, orgID); err != nil {
		t.Fatalf("DeleteTeamsConfig: %v", err)
	}
	if _, err := s.GetTeamsUserEmployee(ctx, orgID, "teams-user-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user map must die with the config: got %v", err)
	}
	if err := s.DeleteTeamsConfig(ctx, orgID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}

	// --- ESS tokens ---
	h := essTokenHash("magic-plaintext-token")
	tok, err := s.IssueESSToken(ctx, ESSToken{
		TokenHash: h, OrganizationID: orgID, EmployeeID: empID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("IssueESSToken: %v", err)
	}
	if tok.Used() {
		t.Fatal("fresh token must not be used")
	}
	if tok.Expired(time.Now()) {
		t.Fatal("fresh token must not be expired")
	}
	gotTok, err := s.GetESSToken(ctx, h)
	if err != nil || gotTok.EmployeeID != empID {
		t.Fatalf("GetESSToken: %+v (%v)", gotTok, err)
	}
	if _, err := s.GetESSToken(ctx, "deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: want ErrNotFound, got %v", err)
	}
	// Digest collision.
	if _, err := s.IssueESSToken(ctx, ESSToken{
		TokenHash: h, OrganizationID: orgID, EmployeeID: empID,
		ExpiresAt: time.Now().Add(15 * time.Minute),
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate digest: want ErrAlreadyExists, got %v", err)
	}
	// Single-use: mark used twice; first wins.
	if err := s.MarkESSTokenUsed(ctx, h, time.Now()); err != nil {
		t.Fatalf("MarkESSTokenUsed: %v", err)
	}
	if err := s.MarkESSTokenUsed(ctx, h, time.Now()); err != nil {
		t.Fatalf("MarkESSTokenUsed idempotent: %v", err)
	}
	gotTok, _ = s.GetESSToken(ctx, h)
	if !gotTok.Used() {
		t.Fatal("token must be marked used")
	}
	// Expiry sweep.
	old := essTokenHash("old-token")
	if _, err := s.IssueESSToken(ctx, ESSToken{
		TokenHash: old, OrganizationID: orgID, EmployeeID: empID,
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("IssueESSToken old: %v", err)
	}
	n, err := s.DeleteExpiredESSTokens(ctx, time.Now())
	if err != nil || n < 1 {
		t.Fatalf("DeleteExpiredESSTokens: n=%d (%v)", n, err)
	}
	if _, err := s.GetESSToken(ctx, old); !errors.Is(err, ErrNotFound) {
		t.Fatalf("swept token must be gone: %v", err)
	}
	if _, err := s.GetESSToken(ctx, h); err != nil {
		t.Fatalf("live token must survive the sweep: %v", err)
	}

	// --- DeleteSlackWorkspace cascades the user map ---
	if err := s.MapSlackUser(ctx, "T123", "U123", empID); err != nil {
		t.Fatalf("remap: %v", err)
	}
	if err := s.DeleteSlackWorkspace(ctx, "T123"); err != nil {
		t.Fatalf("DeleteSlackWorkspace: %v", err)
	}
	if _, err := s.GetSlackUserEmployee(ctx, "T123", "U123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("user map must die with the workspace: %v", err)
	}
	if err := s.DeleteSlackWorkspace(ctx, "T123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete workspace: want ErrNotFound, got %v", err)
	}
}

func TestChatStoreContract_Memory(t *testing.T) {
	testChatStoreContract(t, NewMemoryStore())
}

func TestChatStoreContract_Postgres(t *testing.T) {
	s, _ := testPostgresHRIS(t)
	testChatStoreContract(t, s)
}

// FindEmployeeByEmail is exercised by ESS login; both adapters must agree.
func testFindEmployeeByEmail(t *testing.T, s ChatStore) {
	t.Helper()
	ctx, orgID, empID := chatFixture(t, s)
	full := s.(interface {
		CreateEmployee(ctx context.Context, e Employee) (Employee, error)
	})
	emp, err := full.CreateEmployee(ctx, Employee{
		OrganizationID: orgID, EmployeeNumber: "CHAT-2",
		FirstName: "Login", LastName: "Person",
		Email:  "Login.Person@Example.Test",
		Status: InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	// Case-insensitive match.
	gotOrg, gotEmp, err := s.FindEmployeeByEmail(ctx, "login.person@example.test")
	if err != nil || gotOrg != orgID || gotEmp.ID != emp.ID {
		t.Fatalf("FindEmployeeByEmail: org=%q emp=%q (%v)", gotOrg, gotEmp.ID, err)
	}
	_ = empID
	if _, _, err := s.FindEmployeeByEmail(ctx, "nobody@example.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown email: want ErrNotFound, got %v", err)
	}
	// A non-joined employee is invisible to login.
	if _, err := full.CreateEmployee(ctx, Employee{
		OrganizationID: orgID, EmployeeNumber: "CHAT-3",
		FirstName: "Ghost", Email: "ghost@example.test",
		Status: InvitationPending,
	}); err != nil {
		t.Fatalf("CreateEmployee ghost: %v", err)
	}
	if _, _, err := s.FindEmployeeByEmail(ctx, "ghost@example.test"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-joined email: want ErrNotFound, got %v", err)
	}
}

func TestFindEmployeeByEmail_Memory(t *testing.T) {
	testFindEmployeeByEmail(t, NewMemoryStore())
}

func TestFindEmployeeByEmail_Postgres(t *testing.T) {
	s, _ := testPostgresHRIS(t)
	testFindEmployeeByEmail(t, s)
}

func TestChatStoreConcurrent_SaveWorkspace(t *testing.T) {
	s := NewMemoryStore()
	ctx, orgID, _ := chatFixture(t, s)
	// Concurrent first-saves: exactly one wins the version-1 write; the
	// others conflict.
	const n = 8
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			_, err := s.SaveSlackWorkspace(ctx, SlackWorkspace{
				TeamID: "TC", OrganizationID: orgID,
				EncSigningSecret: "s", EncServiceToken: "t",
			})
			errs <- err
		}(i)
	}
	var ok, conflict int
	for i := 0; i < n; i++ {
		err := <-errs
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrVersionConflict):
			conflict++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("want 1 winner and %d conflicts, got %d/%d", n-1, ok, conflict)
	}
}
