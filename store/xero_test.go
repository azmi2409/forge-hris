package store

// MemoryStore Xero contract tests: connection CRUD with version-checked
// writes, atomic mapping replacement, and the idempotent posting ledger.

import (
	"context"
	"errors"
	"testing"
	"time"
)

func mustXeroOrg(t *testing.T, s *MemoryStore) string {
	t.Helper()
	org, err := s.CreateOrganization(context.Background(), Organization{
		Name: "Xero Co", Slug: "xero-" + newID()[:8], CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org.ID
}

func TestMemoryXeroConnectionCRUD(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	orgID := mustXeroOrg(t, s)

	if _, err := s.GetXeroConnection(ctx, orgID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: want ErrNotFound, got %v", err)
	}

	saved, err := s.SaveXeroConnection(ctx, XeroConnection{
		OrganizationID:  orgID,
		TenantID:        "tenant-1",
		EncAccessToken:  "enc-access",
		EncRefreshToken: "enc-refresh",
		ExpiresAt:       time.Now().Add(time.Hour).UTC(),
	})
	if err != nil {
		t.Fatalf("SaveXeroConnection: %v", err)
	}
	if saved.Version != 1 || saved.AutoPost {
		t.Fatalf("insert: want version 1 and auto_post false, got %+v", saved)
	}

	// Inserting again is a conflict.
	if _, err := s.SaveXeroConnection(ctx, XeroConnection{OrganizationID: orgID}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("re-insert: want ErrAlreadyExists, got %v", err)
	}

	got, err := s.GetXeroConnection(ctx, orgID)
	if err != nil {
		t.Fatalf("GetXeroConnection: %v", err)
	}
	if got.TenantID != "tenant-1" || got.EncAccessToken != "enc-access" {
		t.Fatalf("get mismatch: %+v", got)
	}

	// Stale version is rejected.
	stale := got
	stale.AutoPost = true
	stale.Version = 999
	if _, err := s.SaveXeroConnection(ctx, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: want ErrVersionConflict, got %v", err)
	}

	got.AutoPost = true
	updated, err := s.SaveXeroConnection(ctx, got)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Version != 2 || !updated.AutoPost {
		t.Fatalf("update mismatch: %+v", updated)
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

func TestMemoryXeroConnectionUnknownOrg(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	_, err := s.SaveXeroConnection(ctx, XeroConnection{OrganizationID: newID()})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown org: want ErrNotFound, got %v", err)
	}
}

func TestMemoryXeroMappings(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	orgID := mustXeroOrg(t, s)

	accts, conts, err := s.GetXeroMappings(ctx, orgID)
	if err != nil || len(accts) != 0 || len(conts) != 0 {
		t.Fatalf("empty mappings: %v %v %v", accts, conts, err)
	}

	if err := s.SaveXeroMappings(ctx, orgID,
		[]XeroAccountMapping{{ComponentKey: "payroll.net_pay", AccountCode: "476"}},
		[]XeroContactMapping{{EmployeeID: "emp-1", ContactID: "contact-1"}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	accts, conts, err = s.GetXeroMappings(ctx, orgID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(accts) != 1 || accts[0].AccountCode != "476" || accts[0].OrganizationID != orgID {
		t.Fatalf("accounts: %+v", accts)
	}
	if len(conts) != 1 || conts[0].ContactID != "contact-1" {
		t.Fatalf("contacts: %+v", conts)
	}

	// Replacement is atomic: the old sets are fully swapped.
	if err := s.SaveXeroMappings(ctx, orgID,
		[]XeroAccountMapping{{ComponentKey: "payroll.net_pay", AccountCode: "999"}},
		nil); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	accts, conts, err = s.GetXeroMappings(ctx, orgID)
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if len(accts) != 1 || accts[0].AccountCode != "999" || len(conts) != 0 {
		t.Fatalf("replace: %+v %+v", accts, conts)
	}

	// Mappings die with the connection.
	if _, err := s.SaveXeroConnection(ctx, XeroConnection{OrganizationID: orgID}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := s.DeleteXeroConnection(ctx, orgID); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	accts, conts, err = s.GetXeroMappings(ctx, orgID)
	if err != nil || len(accts) != 0 || len(conts) != 0 {
		t.Fatalf("mappings survive disconnect: %v %v %v", accts, conts, err)
	}

	// Validation.
	if err := s.SaveXeroMappings(ctx, orgID, []XeroAccountMapping{{ComponentKey: "x"}}, nil); err == nil {
		t.Fatal("missing account_code: want error")
	}
}

func TestMemoryXeroBillPostings(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	orgID := mustXeroOrg(t, s)
	runID, empID := newID(), newID()

	if rows, err := s.ListBillPostings(ctx, orgID, runID); err != nil || len(rows) != 0 {
		t.Fatalf("empty: %v %v", rows, err)
	}

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

	// The same (run, employee) cannot be recorded twice — idempotent posts.
	if _, err := s.RecordBillPosting(ctx, XeroBillPosting{
		OrganizationID: orgID, RunID: runID, EmployeeID: empID, Status: XeroBillPostingPosted,
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate: want ErrAlreadyExists, got %v", err)
	}

	// A failed attempt for the same pair is also blocked: the ledger is the
	// single source of truth; a new attempt needs a new employee or run.
	if _, err := s.RecordBillPosting(ctx, XeroBillPosting{
		OrganizationID: orgID, RunID: runID, EmployeeID: empID, Status: XeroBillPostingFailed, Error: "boom",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate failed: want ErrAlreadyExists, got %v", err)
	}

	rows, err := s.ListBillPostings(ctx, orgID, runID)
	if err != nil || len(rows) != 1 || rows[0].XeroInvoiceNumber != "INV-0001" {
		t.Fatalf("list: %+v %v", rows, err)
	}

	// Postings for another run are listed separately.
	other, err := s.RecordBillPosting(ctx, XeroBillPosting{
		OrganizationID: orgID, RunID: newID(), EmployeeID: empID, Status: XeroBillPostingFailed, Error: "down",
	})
	if err != nil {
		t.Fatalf("Record other: %v", err)
	}
	if other.Status != XeroBillPostingFailed || other.Error != "down" {
		t.Fatalf("other: %+v", other)
	}
	rows, err = s.ListBillPostings(ctx, orgID, runID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list after other: %+v %v", rows, err)
	}

	// Validation.
	if _, err := s.RecordBillPosting(ctx, XeroBillPosting{OrganizationID: orgID}); err == nil {
		t.Fatal("missing run_id: want error")
	}
	if _, err := s.RecordBillPosting(ctx, XeroBillPosting{
		OrganizationID: orgID, RunID: runID, EmployeeID: newID(), Status: "bogus",
	}); err == nil {
		t.Fatal("bad status: want error")
	}
}
