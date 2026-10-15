package store

// Postgres adapter tests. They run in CI against a postgres:16 service and
// locally with:
//
//	TEST_DATABASE_URL=postgres://forge:forge@localhost:5432/forge_hris go test ./store/ -run TestPostgres -v
//
// Without TEST_DATABASE_URL they skip cleanly.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// testPostgresHRIS connects to an isolated database per run so parallel CI
// jobs never clash, mirroring the forge-tasks postgres test harness.
func testPostgresHRIS(t *testing.T) (*PostgresStore, context.Context) {
	t.Helper()
	rawurl := os.Getenv("TEST_DATABASE_URL")
	if rawurl == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres adapter tests")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("forge_hris_test_%d", os.Getpid())

	// The service database starts empty; migrate it first so the admin
	// connection (which verifies schema presence) succeeds.
	if err := ApplyMigration(ctx, rawurl); err != nil {
		t.Fatalf("migrate service db: %v", err)
	}
	admin, err := NewPostgresStore(ctx, rawurl)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.pool.Exec(ctx, `CREATE DATABASE `+dbName); err != nil {
		admin.Close()
		t.Fatalf("create db: %v", err)
	}
	admin.Close()
	t.Cleanup(func() {
		admin, _ := NewPostgresStore(ctx, rawurl)
		if admin != nil {
			_, _ = admin.pool.Exec(ctx, `DROP DATABASE IF EXISTS `+dbName)
			admin.Close()
		}
	})

	testURL := withTestDB(t, rawurl, dbName)
	if err := ApplyMigration(ctx, testURL); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s, err := NewPostgresStore(ctx, testURL)
	if err != nil {
		t.Fatalf("connect migrated: %v", err)
	}
	t.Cleanup(s.Close)
	return s, ctx
}

func withTestDB(t *testing.T, rawurl, db string) string {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatalf("bad TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + db
	return u.String()
}

// TestPostgresCoreRoundTrip exercises org/employee/credential/audit through
// the pgx adapter, plus the GetCredential admin read path.
func TestPostgresCoreRoundTrip(t *testing.T) {
	s, ctx := testPostgresHRIS(t)

	org, err := s.CreateOrganization(ctx, Organization{
		Name: "Acme Sdn Bhd", Slug: "acme-pg", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	if _, err := s.CreateOrganization(ctx, Organization{
		Name: "Dupe", Slug: "acme-pg", CountryCode: "MY",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate slug: want ErrAlreadyExists, got %v", err)
	}

	emp, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID,
		EmployeeNumber: "E001",
		FirstName:      "Azmi",
		Email:          "azmi@acme.test",
		Status:         InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	if err := s.SetRole(ctx, org.ID, emp.ID, RoleAdmin, "test"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	cred, err := s.IssueCredential(ctx, Credential{
		OrganizationID: org.ID,
		Kind:           CredentialPAT,
		Name:           "cli",
		EmployeeID:     &emp.ID,
	}, "pg-plaintext-token")
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}
	if cred.TokenHash == "" || cred.TokenHash == "pg-plaintext-token" {
		t.Fatalf("token stored in plaintext")
	}
	byHash, err := s.GetCredentialByTokenHash(ctx, cred.TokenHash)
	if err != nil || byHash.ID != cred.ID {
		t.Fatalf("GetCredentialByTokenHash: %v %+v", err, byHash)
	}
	byID, err := s.GetCredential(ctx, org.ID, cred.ID)
	if err != nil || byID.ID != cred.ID {
		t.Fatalf("GetCredential: %v %+v", err, byID)
	}

	ev1, err := s.AppendAuditEvent(ctx, AuditEvent{
		OrganizationID: org.ID, ActorType: ActorHuman, ActorID: emp.ID,
		Action: "employee.created", ResourceType: "employee", ResourceID: emp.ID,
	})
	if err != nil {
		t.Fatalf("AppendAuditEvent: %v", err)
	}
	ev2, err := s.AppendAuditEvent(ctx, AuditEvent{
		OrganizationID: org.ID, ActorType: ActorHuman, ActorID: emp.ID,
		Action: "role.granted", ResourceType: "membership", ResourceID: emp.ID,
	})
	if err != nil {
		t.Fatalf("AppendAuditEvent 2: %v", err)
	}
	if ev1.PrevHash != "" || ev2.PrevHash != ev1.Hash {
		t.Fatalf("audit chain broken: %q -> %q -> %q", ev1.PrevHash, ev1.Hash, ev2.PrevHash)
	}

	// Optimistic concurrency on Postgres.
	stale := emp
	stale.Version = 999
	if _, err := s.UpdateEmployee(ctx, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: want ErrVersionConflict, got %v", err)
	}
	dead, err := s.DeactivateEmployee(ctx, org.ID, emp.ID, emp.Version)
	if err != nil || dead.Status != InvitationRemoved {
		t.Fatalf("DeactivateEmployee: %v %+v", err, dead)
	}

	// Idempotency round trip.
	rec := IdempotencyRecord{
		OrganizationID: org.ID, Key: "req-1", Fingerprint: "fp",
		StatusCode: 201, ResponseBody: []byte(`{"ok":true}`),
	}
	if err := s.IdempotencyPut(ctx, rec); err != nil {
		t.Fatalf("IdempotencyPut: %v", err)
	}
	back, found, err := s.IdempotencyGet(ctx, org.ID, "req-1")
	if err != nil || !found || back.StatusCode != 201 {
		t.Fatalf("IdempotencyGet: %v found=%v %+v", err, found, back)
	}
}

// TestPostgresTenantIsolation verifies that tenant A's operations never see
// tenant B's rows: adapter-level (explicit predicates) and, via a non-owner
// probe role, that the RLS policies in 001_init.sql actually enforce the
// isolation they declare.
func TestPostgresTenantIsolation(t *testing.T) {
	s, ctx := testPostgresHRIS(t)

	orgA, err := s.CreateOrganization(ctx, Organization{
		Name: "Org A", Slug: "org-a-iso", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization A: %v", err)
	}
	orgB, err := s.CreateOrganization(ctx, Organization{
		Name: "Org B", Slug: "org-b-iso", CountryCode: "ID",
	})
	if err != nil {
		t.Fatalf("CreateOrganization B: %v", err)
	}

	empB, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: orgB.ID, EmployeeNumber: "E-B1", FirstName: "Bee",
		Status: InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee B: %v", err)
	}
	credB, err := s.IssueCredential(ctx, Credential{
		OrganizationID: orgB.ID, Kind: CredentialServiceAccount,
		Name: "svc-b", Scopes: []string{ScopeTimeRead},
	}, "b-plaintext")
	if err != nil {
		t.Fatalf("IssueCredential B: %v", err)
	}

	// Adapter-level isolation: org A's reads must not see org B's rows.
	if _, err := s.GetEmployee(ctx, orgA.ID, empB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetEmployee cross-org: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetCredential(ctx, orgA.ID, credB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetCredential cross-org: want ErrNotFound, got %v", err)
	}
	emps, _, err := s.ListEmployees(ctx, orgA.ID, EmployeeFilter{}, Page{})
	if err != nil {
		t.Fatalf("ListEmployees A: %v", err)
	}
	for _, e := range emps {
		if e.OrganizationID == orgB.ID {
			t.Fatalf("ListEmployees(A) leaked org B employee %s", e.ID)
		}
	}
	if _, err := s.GetCredentialByTokenHash(ctx, credB.TokenHash); err != nil {
		t.Fatalf("GetCredentialByTokenHash (global token lookup): %v", err)
	}

	// RLS policy enforcement probe: connect as a non-owner role (RLS
	// policies are bypassed for table owners, so the probe must not be one)
	// and confirm a bare table scan inside tenant A sees only A's rows.
	probeRole := fmt.Sprintf("rls_probe_%d", os.Getpid())
	probePass := "probe-secret"
	dbName := fmt.Sprintf("forge_hris_test_%d", os.Getpid())
	u, _ := url.Parse(os.Getenv("TEST_DATABASE_URL"))
	u.Path = "/" + dbName
	u.User = url.UserPassword(probeRole, probePass)

	if _, err := s.pool.Exec(ctx, fmt.Sprintf(
		`DO $$ BEGIN
			IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '%[1]s') THEN
				CREATE ROLE %[2]s LOGIN;
			END IF;
		END $$`, probeRole, quoteIdent(probeRole))); err != nil {
		t.Fatalf("create probe role: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`GRANT USAGE ON SCHEMA public TO `+quoteIdent(probeRole)+`; `+
			// organizations is included so the probe's NewPostgresStore schema
			// check (information_schema only shows tables the role can see)
			// succeeds; the probe only ever scans employees.
			`GRANT SELECT ON organizations, employees, credentials, audit_events TO `+quoteIdent(probeRole)); err != nil {
		t.Fatalf("grant probe: %v", err)
	}
	if _, err := s.pool.Exec(ctx,
		`ALTER ROLE `+quoteIdent(probeRole)+` WITH LOGIN PASSWORD '`+probePass+`'`); err != nil {
		t.Fatalf("set probe login: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+quoteIdent(probeRole))
	})

	probe, err := NewPostgresStore(ctx, u.String())
	if err != nil {
		t.Fatalf("connect as probe: %v", err)
	}
	defer probe.Close()

	var countB int
	err = probe.inTenant(ctx, orgA.ID, func(tx pgx.Tx) error {
		// No explicit organization_id predicate: only RLS stands between
		// tenant A and org B's rows.
		return tx.QueryRow(ctx, `SELECT count(*) FROM employees`).Scan(&countB)
	})
	if err != nil {
		t.Fatalf("probe scan: %v", err)
	}
	var countA int
	err = probe.inTenant(ctx, orgA.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM employees WHERE organization_id = $1::uuid`, orgA.ID).Scan(&countA)
	})
	if err != nil {
		t.Fatalf("probe scan A: %v", err)
	}
	if countB != countA {
		t.Fatalf("RLS probe: bare scan in tenant A saw %d rows, org A has %d (isolation broken)", countB, countA)
	}
}

func quoteIdent(s string) string {
	return `"` + s + `"`
}

// TestPostgresAuditChainRace hammers the chain from concurrent appends; the
// FOR UPDATE tail lock must keep it linear (exactly one history, no forks).
func TestPostgresAuditChainRace(t *testing.T) {
	s, ctx := testPostgresHRIS(t)

	org, err := s.CreateOrganization(ctx, Organization{
		Name: "Race PG", Slug: "race-pg", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}

	const workers = 8
	const perWorker = 10
	done := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		go func(w int) {
			for i := 0; i < perWorker; i++ {
				_, err := s.AppendAuditEvent(ctx, AuditEvent{
					OrganizationID: org.ID,
					ActorType:      ActorAgent,
					ActorID:        fmt.Sprintf("agent-%d", w),
					Action:         "test.append",
					ResourceType:   "test",
				})
				done <- err
			}
		}(w)
	}
	for i := 0; i < workers*perWorker; i++ {
		if err := <-done; err != nil {
			t.Fatalf("AppendAuditEvent: %v", err)
		}
	}

	events, _, err := s.ListAuditEvents(ctx, org.ID, AuditFilter{}, Page{Limit: MaxListLimit})
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	if len(events) != workers*perWorker {
		t.Fatalf("want %d events, got %d", workers*perWorker, len(events))
	}
	seen := make(map[int64]bool, len(events))
	prev := ""
	for _, ev := range events {
		if seen[ev.ID] {
			t.Fatalf("duplicate audit id %d (chain forked)", ev.ID)
		}
		seen[ev.ID] = true
		_ = prev
		prev = ev.Hash
	}
	// Verify linkage in ID order (Postgres BIGSERIAL ids are append-ordered).
	for i := 1; i < len(events); i++ {
		a, b := events[i-1], events[i]
		if a.ID+1 != b.ID {
			t.Fatalf("audit ids not contiguous: %d then %d", a.ID, b.ID)
		}
		if b.PrevHash != a.Hash {
			t.Fatalf("chain broken at %d: prev %q != %q", b.ID, b.PrevHash, a.Hash)
		}
	}
	if events[0].PrevHash != "" {
		t.Fatalf("first event PrevHash = %q, want empty", events[0].PrevHash)
	}
}
