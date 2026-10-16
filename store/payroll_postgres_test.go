package store

// Postgres adapter tests for the payroll store (component 5). Same core
// paths as the memory tests: preset catalog merge + immutability, run
// lifecycle + illegal transitions, pay-line upsert versioning, sync-item
// draft gate, preview expiry, cross-org isolation. Skips cleanly without
// TEST_DATABASE_URL.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/payroll"
)

func mustPgPayrollOrg(t *testing.T, s *PostgresStore, name string) Organization {
	t.Helper()
	org, err := s.CreateOrganization(context.Background(), Organization{
		Name:        name,
		Slug:        fmt.Sprintf("%s-%d", name, time.Now().UnixNano()),
		CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org
}

func mustPgPayrollEmployee(t *testing.T, s *PostgresStore, orgID, number string) Employee {
	t.Helper()
	emp, err := s.CreateEmployee(context.Background(), Employee{
		OrganizationID: orgID,
		EmployeeNumber: number,
		FirstName:      "Pay",
		LastName:       number,
		Status:         InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	return emp
}

func TestPostgresPayrollPresets(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	org := mustPgPayrollOrg(t, s, "pgpreset")

	seeds := payroll.SeedPresets()
	got, _, err := s.ListPayrollPresets(ctx, org.ID, Page{})
	if err != nil {
		t.Fatalf("ListPayrollPresets: %v", err)
	}
	if len(got) != len(seeds) {
		t.Fatalf("want %d seeds, got %d", len(seeds), len(got))
	}
	for _, pr := range got {
		if !pr.IsSystem || pr.ID != "sys:"+pr.Code {
			t.Fatalf("seed row wrong: %+v", pr)
		}
	}
	sys, err := s.GetPayrollPreset(ctx, org.ID, "sys:"+seeds[0].Code)
	if err != nil || !sys.IsSystem {
		t.Fatalf("GetPayrollPreset sys: %v", sys)
	}

	orgPr, err := s.CreatePayrollPreset(ctx, org.ID, PayrollItemPreset{
		Code: "PG-BONUS", Name: "PG bonus", Kind: payroll.PresetKind("deduction"),
	})
	if err != nil {
		t.Fatalf("CreatePayrollPreset: %v", err)
	}
	if _, err := s.CreatePayrollPreset(ctx, org.ID, PayrollItemPreset{
		Code: seeds[0].Code, Name: "clash", Kind: payroll.PresetKind("addition"),
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("seed-code create: want ErrAlreadyExists, got %v", err)
	}
	if _, err := s.CreatePayrollPreset(ctx, org.ID, PayrollItemPreset{
		Code: "PG-BONUS", Name: "dup", Kind: payroll.PresetKind("addition"),
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("dup org code: want ErrAlreadyExists, got %v", err)
	}
	if _, err := s.UpdatePayrollPreset(ctx, org.ID, sys); !errors.Is(err, ErrSystemPresetImmutable) {
		t.Errorf("system update: want ErrSystemPresetImmutable, got %v", err)
	}
	orgPr.Name = "renamed"
	updated, err := s.UpdatePayrollPreset(ctx, org.ID, orgPr)
	if err != nil {
		t.Fatalf("UpdatePayrollPreset: %v", err)
	}
	if updated.Version != 2 || updated.Name != "renamed" {
		t.Errorf("update wrong: %+v", updated)
	}
	stale := orgPr
	if _, err := s.UpdatePayrollPreset(ctx, org.ID, stale); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale update: want ErrVersionConflict, got %v", err)
	}

	merged, _, err := s.ListPayrollPresets(ctx, org.ID, Page{})
	if err != nil || len(merged) != len(seeds)+1 {
		t.Fatalf("merged list: got %d, err %v", len(merged), err)
	}
	if merged[len(seeds)].ID != orgPr.ID {
		t.Error("org preset not listed after seeds")
	}
}

func TestPostgresPayrollRunsAndLines(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	org := mustPgPayrollOrg(t, s, "pgrun")
	other := mustPgPayrollOrg(t, s, "pgrun2")
	emp := mustPgPayrollEmployee(t, s, org.ID, "E-1")

	run, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: org.ID, Period: "2026-10", CountryCode: "MY",
		Status: PayrollRunApproved, // forced back to draft
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	if run.Status != PayrollRunDraft {
		t.Errorf("status not forced to draft: %s", run.Status)
	}
	if _, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: org.ID, Period: "2026-10", CountryCode: "MY",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("dup period: want ErrAlreadyExists, got %v", err)
	}
	if _, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: org.ID, Period: "bogus", CountryCode: "MY",
	}); err == nil {
		t.Error("bad period accepted")
	}

	// Same period in another org is fine (cross-org isolation setup).
	if _, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: other.ID, Period: "2026-10", CountryCode: "MY",
	}); err != nil {
		t.Fatalf("other-org period: %v", err)
	}

	line, err := s.UpsertPayLine(ctx, EmployeePayLine{
		OrganizationID: org.ID, RunID: run.ID, EmployeeID: emp.ID,
		GrossMinor: 700000, AdditionsMinor: 700000, DeductionsMinor: 70000, NetMinor: 630000,
		Items: []payroll.PayLineItem{
			{PresetCode: "BASIC", Name: "Basic", Kind: payroll.PresetKind("addition"), AmountMinor: 700000, Note: "n"},
		},
		Statutory:   payroll.StatutorySplits{PCB: 12000, BPJSKesEmployee: 0},
		BankAccount: "777", BankCode: "BCA",
	})
	if err != nil {
		t.Fatalf("UpsertPayLine insert: %v", err)
	}
	if line.Version != 1 {
		t.Errorf("insert version = %d", line.Version)
	}
	got, err := s.GetPayLine(ctx, org.ID, run.ID, emp.ID)
	if err != nil {
		t.Fatalf("GetPayLine: %v", err)
	}
	if got.NetMinor != 630000 || got.Statutory.PCB != 12000 || len(got.Items) != 1 ||
		got.Items[0].Note != "n" || got.BankAccount != "777" {
		t.Errorf("round trip mismatch: %+v", got)
	}

	got.NetMinor = 640000
	updated, err := s.UpsertPayLine(ctx, got)
	if err != nil {
		t.Fatalf("UpsertPayLine update: %v", err)
	}
	if updated.Version != 2 {
		t.Errorf("update version = %d", updated.Version)
	}
	if _, err := s.UpsertPayLine(ctx, got); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale upsert: want ErrVersionConflict, got %v", err)
	}

	// Cross-org isolation: other org sees nothing.
	if _, err := s.GetPayLine(ctx, other.ID, run.ID, emp.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-org pay line: want ErrNotFound, got %v", err)
	}
	lines, _, err := s.ListPayLines(ctx, other.ID, run.ID, Page{})
	if err != nil || len(lines) != 0 {
		t.Errorf("cross-org list lines: got %d, err %v", len(lines), err)
	}

	// Lifecycle: draft -> approved -> locked; illegal moves rejected.
	if _, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, run.Version, PayrollRunLocked, "me"); !errors.Is(err, ErrIllegalRunTransition) {
		t.Errorf("draft->locked: want ErrIllegalRunTransition, got %v", err)
	}
	approved, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, run.Version, PayrollRunApproved, "me")
	if err != nil {
		t.Fatalf("draft->approved: %v", err)
	}
	if _, err := s.UpsertPayLine(ctx, EmployeePayLine{
		OrganizationID: org.ID, RunID: run.ID, EmployeeID: emp.ID, Version: updated.Version,
	}); !errors.Is(err, ErrRunNotDraft) {
		t.Errorf("approved run upsert: want ErrRunNotDraft, got %v", err)
	}
	locked, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, approved.Version, PayrollRunLocked, "me")
	if err != nil {
		t.Fatalf("approved->locked: %v", err)
	}
	if locked.Status != PayrollRunLocked {
		t.Errorf("expected locked, got %s", locked.Status)
	}
	if _, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, locked.Version, PayrollRunDraft, "me"); !errors.Is(err, ErrIllegalRunTransition) {
		t.Errorf("locked->draft: want ErrIllegalRunTransition, got %v", err)
	}
}

func TestPostgresPayrollSyncAndPreview(t *testing.T) {
	s, ctx := testPostgresHRIS(t)
	org := mustPgPayrollOrg(t, s, "pgsync")
	emp := mustPgPayrollEmployee(t, s, org.ID, "E-1")

	run, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: org.ID, Period: "2026-10", CountryCode: "ID",
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	if run.CountryCode != "ID" {
		t.Errorf("country not stored: %s", run.CountryCode)
	}

	if err := s.AddSyncItems(ctx, org.ID, run.ID, nil); err != nil {
		t.Fatalf("empty add: %v", err)
	}
	err = s.AddSyncItems(ctx, org.ID, run.ID, []SyncItem{
		{EmployeeID: emp.ID, Source: "att", Label: "OT", Kind: payroll.PresetKind("addition"), PresetCode: "OT", AmountMinor: 15000},
		{EmployeeID: emp.ID, Source: "att", ExternalID: "x-2", Label: "Late", Kind: payroll.PresetKind("deduction"), AmountMinor: 2000},
	})
	if err != nil {
		t.Fatalf("AddSyncItems: %v", err)
	}
	items, _, err := s.ListSyncItems(ctx, org.ID, run.ID, Page{})
	if err != nil || len(items) != 2 {
		t.Fatalf("ListSyncItems: got %d, err %v", len(items), err)
	}
	if items[0].PresetCode != "OT" || items[1].ExternalID != "x-2" {
		t.Errorf("sync item fields wrong: %+v", items)
	}
	// Atomic: one bad item -> nothing added.
	bad := []SyncItem{
		{EmployeeID: emp.ID, Kind: payroll.PresetKind("addition"), AmountMinor: 1},
		{EmployeeID: emp.ID, Kind: payroll.PresetKind("bogus"), AmountMinor: 2},
	}
	if err := s.AddSyncItems(ctx, org.ID, run.ID, bad); err == nil {
		t.Fatal("bad sync item accepted")
	}
	items, _, _ = s.ListSyncItems(ctx, org.ID, run.ID, Page{})
	if len(items) != 2 {
		t.Errorf("atomicity broken: %d items", len(items))
	}

	if _, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, run.Version, PayrollRunApproved, "me"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := s.AddSyncItems(ctx, org.ID, run.ID, []SyncItem{
		{EmployeeID: emp.ID, Kind: payroll.PresetKind("addition"), AmountMinor: 1},
	}); !errors.Is(err, ErrRunNotDraft) {
		t.Errorf("approved run add: want ErrRunNotDraft, got %v", err)
	}

	// Previews.
	saved, err := s.SavePayrollPreview(ctx, PayrollPreview{
		OrganizationID: org.ID, RunID: run.ID,
		Payload:   []byte(`{"net":630000}`),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("SavePayrollPreview: %v", err)
	}
	prev, err := s.GetPayrollPreview(ctx, org.ID, saved.ID)
	if err != nil {
		t.Fatalf("GetPayrollPreview: %v", err)
	}
	if string(prev.Payload) != `{"net":630000}` {
		t.Errorf("payload mismatch: %s", prev.Payload)
	}
	expired, err := s.SavePayrollPreview(ctx, PayrollPreview{
		OrganizationID: org.ID, RunID: run.ID,
		Payload:   []byte(`{}`),
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("SavePayrollPreview expired: %v", err)
	}
	if _, err := s.GetPayrollPreview(ctx, org.ID, expired.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired preview: want ErrNotFound, got %v", err)
	}

	// Runs list with filters.
	runs, _, err := s.ListPayrollRuns(ctx, org.ID, PayrollRunFilter{Status: PayrollRunApproved}, Page{})
	if err != nil || len(runs) != 1 || !strings.HasSuffix(runs[0].Period, "2026-10") {
		t.Errorf("status filter: got %+v, err %v", runs, err)
	}
}
