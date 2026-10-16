package store

// Postgres adapter Xero tests: the same contract as the MemoryStore
// (connection CRUD, version-checked writes, mappings, posting ledger), run
// against a real database. They skip cleanly without TEST_DATABASE_URL,
// using the testPostgresHRIS helper from postgres_test.go (same package).

import (
	"errors"
	"testing"
	"time"
)

func mustPgXeroOrg(t *testing.T, s *PostgresStore) string {
	t.Helper()
	org, err := s.CreateOrganization(t.Context(), Organization{
		Name: "PG Xero Co", Slug: "pg-xero-" + newID()[:8], CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org.ID
}

func mustPgXeroEmployee(t *testing.T, s *PostgresStore, orgID, number string) Employee {
	t.Helper()
	emp, err := s.CreateEmployee(t.Context(), Employee{
		OrganizationID: orgID,
		EmployeeNumber: number,
		FirstName:      "Xero",
		LastName:       "Test",
		Email:          number + "@example.test",
		Status:         InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	return emp
}

func mustPgXeroRun(t *testing.T, s *PostgresStore, orgID, period string) PayrollRun {
	t.Helper()
	run, err := s.CreatePayrollRun(t.Context(), PayrollRun{
		OrganizationID: orgID,
		Period:         period,
		CountryCode:    "MY",
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	return run
}

func TestPostgresXeroConnectionCRUD(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	orgID := mustPgXeroOrg(t, s)

	if _, err := s.GetXeroConnection(ctx, orgID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: want ErrNotFound, got %v", err)
	}

	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	saved, err := s.SaveXeroConnection(ctx, XeroConnection{
		OrganizationID:  orgID,
		TenantID:        "tenant-1",
		EncAccessToken:  "enc-access",
		EncRefreshToken: "enc-refresh",
		ExpiresAt:       expires,
	})
	if err != nil {
		t.Fatalf("SaveXeroConnection: %v", err)
	}
	if saved.Version != 1 {
		t.Fatalf("insert: want version 1, got %+v", saved)
	}

	got, err := s.GetXeroConnection(ctx, orgID)
	if err != nil {
		t.Fatalf("GetXeroConnection: %v", err)
	}
	if got.TenantID != "tenant-1" || got.EncRefreshToken != "enc-refresh" ||
		!got.ExpiresAt.Equal(expires) {
		t.Fatalf("get mismatch: %+v", got)
	}

	// NULL expires_at reads as the zero time.
	nullSaved, err := s.SaveXeroConnection(ctx, XeroConnection{
		OrganizationID: orgID, TenantID: "t", AutoPost: true, Version: got.Version,
	})
	if err != nil {
		t.Fatalf("update null expiry: %v", err)
	}
	got2, err := s.GetXeroConnection(ctx, orgID)
	if err != nil || !got2.ExpiresAt.IsZero() || !got2.AutoPost || got2.Version != nullSaved.Version {
		t.Fatalf("null expiry round trip: %+v %v", got2, err)
	}

	// Stale version is rejected.
	stale := got2
	stale.Version = 1
	stale.AutoPost = false
	if _, err := s.SaveXeroConnection(ctx, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: want ErrVersionConflict, got %v", err)
	}

	// Duplicate insert is a conflict.
	if _, err := s.SaveXeroConnection(ctx, XeroConnection{OrganizationID: orgID}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("re-insert: want ErrAlreadyExists, got %v", err)
	}

	if err := s.DeleteXeroConnection(ctx, orgID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.GetXeroConnection(ctx, orgID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
	// Delete is idempotent.
	if err := s.DeleteXeroConnection(ctx, orgID); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestPostgresXeroMappings(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	orgID := mustPgXeroOrg(t, s)

	emp := mustPgXeroEmployee(t, s, orgID, "PX-001")
	if err := s.SaveXeroMappings(ctx, orgID,
		[]XeroAccountMapping{
			{ComponentKey: "payroll.net_pay", AccountCode: "476"},
			{ComponentKey: "bonus", AccountCode: "477"},
		},
		[]XeroContactMapping{{EmployeeID: emp.ID, ContactID: "c-1"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	accts, conts, err := s.GetXeroMappings(ctx, orgID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(accts) != 2 || accts[0].ComponentKey != "bonus" || accts[1].AccountCode != "476" {
		t.Fatalf("accounts (want key order): %+v", accts)
	}
	if len(conts) != 1 || conts[0].ContactID != "c-1" {
		t.Fatalf("contacts: %+v", conts)
	}

	// Replacement is atomic.
	if err := s.SaveXeroMappings(ctx, orgID, nil, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	accts, conts, err = s.GetXeroMappings(ctx, orgID)
	if err != nil || len(accts) != 0 || len(conts) != 0 {
		t.Fatalf("cleared: %+v %+v %v", accts, conts, err)
	}

	if err := s.SaveXeroMappings(ctx, newID(), nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown org: want ErrNotFound, got %v", err)
	}
}

func TestPostgresXeroBillPostings(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	orgID := mustPgXeroOrg(t, s)
	emp := mustPgXeroEmployee(t, s, orgID, "PX-002")
	run := mustPgXeroRun(t, s, orgID, "2026-10")
	runID, empID := run.ID, emp.ID

	p, err := s.RecordBillPosting(ctx, XeroBillPosting{
		OrganizationID:    orgID,
		RunID:             runID,
		EmployeeID:        empID,
		Status:            XeroBillPostingPosted,
		XeroInvoiceID:     "inv-1",
		XeroInvoiceNumber: "INV-0001",
		IdempotencyKey:    "auto:" + runID + ":" + empID,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if p.Version != 1 || p.ID == "" {
		t.Fatalf("recorded: %+v", p)
	}

	// The DB unique index makes a duplicate (run, employee) a conflict.
	if _, err := s.RecordBillPosting(ctx, XeroBillPosting{
		OrganizationID: orgID, RunID: runID, EmployeeID: empID, Status: XeroBillPostingFailed, Error: "boom",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate: want ErrAlreadyExists, got %v", err)
	}

	rows, err := s.ListBillPostings(ctx, orgID, runID)
	if err != nil || len(rows) != 1 || rows[0].XeroInvoiceNumber != "INV-0001" {
		t.Fatalf("list: %+v %v", rows, err)
	}
}

func TestPostgresXeroDisconnectDropsMappings(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	orgID := mustPgXeroOrg(t, s)

	if _, err := s.SaveXeroConnection(ctx, XeroConnection{OrganizationID: orgID}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := s.SaveXeroMappings(ctx, orgID,
		[]XeroAccountMapping{{ComponentKey: "payroll.net_pay", AccountCode: "476"}}, nil); err != nil {
		t.Fatalf("mappings: %v", err)
	}
	if err := s.DeleteXeroConnection(ctx, orgID); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	accts, conts, err := s.GetXeroMappings(ctx, orgID)
	if err != nil || len(accts) != 0 || len(conts) != 0 {
		t.Fatalf("mappings survive disconnect: %+v %+v %v", accts, conts, err)
	}
}
