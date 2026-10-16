package store

// Payroll store tests (component 5): the MemoryStore implementation —
// preset catalog merge + immutability, run lifecycle, pay-line upsert
// versioning, sync-item draft gate + atomicity, preview expiry,
// pagination, and cross-org isolation.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/azmi2409/forge-hris/payroll"
)

func payrollCtx() context.Context { return context.Background() }

func mustPayrollOrg(t *testing.T, s *MemoryStore, name string) Organization {
	t.Helper()
	org, err := s.CreateOrganization(payrollCtx(), Organization{
		Name:        name,
		Slug:        strings.ToLower(name) + "-" + strings.ToLower(newID()[:8]),
		CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org
}

func mustPayrollEmployee(t *testing.T, s *MemoryStore, orgID, number string) Employee {
	t.Helper()
	emp, err := s.CreateEmployee(payrollCtx(), Employee{
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

func mustPayrollRun(t *testing.T, s *MemoryStore, orgID, period string) PayrollRun {
	t.Helper()
	run, err := s.CreatePayrollRun(payrollCtx(), PayrollRun{
		OrganizationID:  orgID,
		Period:          period,
		CountryCode:     "MY",
		RulePackVersion: "2026.1",
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	return run
}

func seedCode(t *testing.T) string {
	t.Helper()
	seeds := payroll.SeedPresets()
	if len(seeds) == 0 {
		t.Fatal("SeedPresets returned no presets")
	}
	return seeds[0].Code
}

// --- pure bits ---

func TestAllowedRunTransition(t *testing.T) {
	cases := []struct {
		from, to PayrollRunStatus
		want     bool
	}{
		{PayrollRunDraft, PayrollRunApproved, true},
		{PayrollRunDraft, PayrollRunCancelled, true},
		{PayrollRunDraft, PayrollRunLocked, false},
		{PayrollRunDraft, PayrollRunDraft, false},
		{PayrollRunApproved, PayrollRunLocked, true},
		{PayrollRunApproved, PayrollRunCancelled, true},
		{PayrollRunApproved, PayrollRunDraft, false},
		{PayrollRunApproved, PayrollRunApproved, false},
		{PayrollRunLocked, PayrollRunCancelled, false},
		{PayrollRunLocked, PayrollRunDraft, false},
		{PayrollRunCancelled, PayrollRunDraft, false},
	}
	for _, tc := range cases {
		if got := allowedRunTransition(tc.from, tc.to); got != tc.want {
			t.Errorf("allowedRunTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestPayrollRunValidate(t *testing.T) {
	base := PayrollRun{OrganizationID: "o", Period: "2026-10", CountryCode: "MY"}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid run rejected: %v", err)
	}
	for period, ok := range map[string]bool{
		"2026-10": true, "2026-1": false, "2026-13": false, "26-10": false, "": false,
	} {
		r := base
		r.Period = period
		if err := r.Validate(); (err == nil) != ok {
			t.Errorf("period %q: ok=%v, err=%v", period, ok, err)
		}
	}
	r := base
	r.CountryCode = "SG"
	if err := r.Validate(); err == nil {
		t.Error("country SG accepted")
	}
}

func TestPresetConversionsRoundTrip(t *testing.T) {
	seeds := payroll.SeedPresets()
	if len(seeds) == 0 {
		t.Skip("no seed presets to convert")
	}
	in := seeds[0]
	pr := PayrollToPreset(in)
	if pr.IsSystem || pr.ID != "" || pr.Version != 0 {
		t.Errorf("PayrollToPreset leaked store fields: %+v", pr)
	}
	back := PresetToPayroll(pr)
	if back != in {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", back, in)
	}
}

// --- presets ---

func TestPayrollPresetMerge(t *testing.T) {
	s := NewMemoryStore()
	ctx := payrollCtx()
	org := mustPayrollOrg(t, s, "presetco")

	got, _, err := s.ListPayrollPresets(ctx, org.ID, Page{})
	if err != nil {
		t.Fatalf("ListPayrollPresets: %v", err)
	}
	nSeeds := len(payroll.SeedPresets())
	if len(got) != nSeeds {
		t.Fatalf("want %d seed presets, got %d", nSeeds, len(got))
	}
	for i, pr := range got {
		if !pr.IsSystem || !strings.HasPrefix(pr.ID, "sys:") {
			t.Errorf("preset %d not marked system: %+v", i, pr)
		}
		if pr.ID != "sys:"+pr.Code {
			t.Errorf("preset %d ID %q != sys:+code", i, pr.ID)
		}
	}

	// System presets resolve by sys: ID too.
	sys, err := s.GetPayrollPreset(ctx, org.ID, "sys:"+seedCode(t))
	if err != nil {
		t.Fatalf("GetPayrollPreset sys: %v", err)
	}
	if !sys.IsSystem {
		t.Error("resolved system preset not marked system")
	}

	// Create an org preset: it lands AFTER the seeds.
	orgPr, err := s.CreatePayrollPreset(ctx, org.ID, PayrollItemPreset{
		Code: "ORG-BONUS", Name: "Org bonus", Kind: payroll.PresetKind("addition"),
		SubjectToEPF: true,
	})
	if err != nil {
		t.Fatalf("CreatePayrollPreset: %v", err)
	}
	if orgPr.IsSystem || orgPr.Version != 1 {
		t.Errorf("org preset flags wrong: %+v", orgPr)
	}
	got, _, err = s.ListPayrollPresets(ctx, org.ID, Page{})
	if err != nil {
		t.Fatalf("ListPayrollPresets: %v", err)
	}
	if len(got) != nSeeds+1 || got[nSeeds].ID != orgPr.ID {
		t.Errorf("org preset not listed after seeds: got %d rows", len(got))
	}

	// Seed-code collision is rejected on create...
	if _, err := s.CreatePayrollPreset(ctx, org.ID, PayrollItemPreset{
		Code: seedCode(t), Name: "clash", Kind: payroll.PresetKind("addition"),
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("seed-code create: want ErrAlreadyExists, got %v", err)
	}
	// ...and on code change.
	orgPr.Code = seedCode(t)
	if _, err := s.UpdatePayrollPreset(ctx, org.ID, orgPr); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("seed-code update: want ErrAlreadyExists, got %v", err)
	}

	// System presets are immutable.
	if _, err := s.UpdatePayrollPreset(ctx, org.ID, sys); !errors.Is(err, ErrSystemPresetImmutable) {
		t.Errorf("system update: want ErrSystemPresetImmutable, got %v", err)
	}

	// Org preset version conflict.
	stale := orgPr
	stale.Name = "renamed"
	stale.Version = 999
	if _, err := s.UpdatePayrollPreset(ctx, org.ID, stale); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale update: want ErrVersionConflict, got %v", err)
	}
	updated, err := s.UpdatePayrollPreset(ctx, org.ID, PayrollItemPreset{
		ID: orgPr.ID, OrganizationID: org.ID, Code: "ORG-BONUS", Name: "renamed",
		Kind: payroll.PresetKind("addition"), Version: orgPr.Version,
	})
	if err != nil {
		t.Fatalf("UpdatePayrollPreset: %v", err)
	}
	if updated.Version != 2 || updated.Name != "renamed" {
		t.Errorf("update result wrong: %+v", updated)
	}

	// Unknown preset.
	if _, err := s.GetPayrollPreset(ctx, org.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown preset: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetPayrollPreset(ctx, org.ID, "sys:does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sys preset: want ErrNotFound, got %v", err)
	}
}

// --- runs ---

func TestPayrollRunLifecycle(t *testing.T) {
	s := NewMemoryStore()
	ctx := payrollCtx()
	org := mustPayrollOrg(t, s, "runco")
	org2 := mustPayrollOrg(t, s, "runco2")

	// Status is forced to draft even when the caller passes approved.
	run, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: org.ID, Period: "2026-10", CountryCode: "MY",
		Status: PayrollRunApproved,
	})
	if err != nil {
		t.Fatalf("CreatePayrollRun: %v", err)
	}
	if run.Status != PayrollRunDraft || run.Version != 1 {
		t.Errorf("create forced wrong state: %+v", run)
	}

	// Bad period rejected.
	if _, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: org.ID, Period: "October", CountryCode: "MY",
	}); err == nil {
		t.Error("bad period accepted")
	}

	// Duplicate (org, period) rejected; same period in another org is fine.
	if _, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: org.ID, Period: "2026-10", CountryCode: "MY",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("dup period: want ErrAlreadyExists, got %v", err)
	}
	if _, err := s.CreatePayrollRun(ctx, PayrollRun{
		OrganizationID: org2.ID, Period: "2026-10", CountryCode: "MY",
	}); err != nil {
		t.Errorf("same period other org rejected: %v", err)
	}

	// draft -> locked is illegal.
	if _, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, run.Version, PayrollRunLocked, "me"); !errors.Is(err, ErrIllegalRunTransition) {
		t.Errorf("draft->locked: want ErrIllegalRunTransition, got %v", err)
	}
	// draft -> approved works.
	approved, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, run.Version, PayrollRunApproved, "me")
	if err != nil {
		t.Fatalf("draft->approved: %v", err)
	}
	if approved.Version != 2 || approved.Status != PayrollRunApproved {
		t.Errorf("transition result wrong: %+v", approved)
	}
	// Stale version.
	if _, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, run.Version, PayrollRunLocked, "me"); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale version: want ErrVersionConflict, got %v", err)
	}
	// approved -> locked works.
	locked, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, approved.Version, PayrollRunLocked, "me")
	if err != nil {
		t.Fatalf("approved->locked: %v", err)
	}
	if locked.Status != PayrollRunLocked {
		t.Errorf("expected locked, got %s", locked.Status)
	}
	// locked -> cancelled is illegal.
	if _, err := s.TransitionPayrollRun(ctx, org.ID, locked.ID, locked.Version, PayrollRunCancelled, "me"); !errors.Is(err, ErrIllegalRunTransition) {
		t.Errorf("locked->cancelled: want ErrIllegalRunTransition, got %v", err)
	}
	// Unknown run.
	if _, err := s.TransitionPayrollRun(ctx, org.ID, "nope", 1, PayrollRunApproved, "me"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown run: want ErrNotFound, got %v", err)
	}
	// Unknown target status.
	if _, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, 99, PayrollRunStatus("bogus"), "me"); err == nil {
		t.Error("bogus target status accepted")
	}

	// draft -> cancelled is legal on a fresh run.
	r2 := mustPayrollRun(t, s, org.ID, "2026-11")
	cancelled, err := s.TransitionPayrollRun(ctx, org.ID, r2.ID, r2.Version, PayrollRunCancelled, "me")
	if err != nil {
		t.Fatalf("draft->cancelled: %v", err)
	}
	if cancelled.Status != PayrollRunCancelled {
		t.Errorf("expected cancelled, got %s", cancelled.Status)
	}

	// Listing + filters.
	runs, _, err := s.ListPayrollRuns(ctx, org.ID, PayrollRunFilter{}, Page{})
	if err != nil {
		t.Fatalf("ListPayrollRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Errorf("want 2 runs, got %d", len(runs))
	}
	drafts, _, err := s.ListPayrollRuns(ctx, org.ID, PayrollRunFilter{Status: PayrollRunDraft}, Page{})
	if err != nil || len(drafts) != 0 {
		t.Errorf("draft filter: got %d runs, err %v", len(drafts), err)
	}
	byPeriod, _, err := s.ListPayrollRuns(ctx, org.ID, PayrollRunFilter{Period: "2026-10"}, Page{})
	if err != nil || len(byPeriod) != 1 {
		t.Errorf("period filter: got %d runs, err %v", len(byPeriod), err)
	}
}

// --- pay lines ---

func TestPayLineUpsert(t *testing.T) {
	s := NewMemoryStore()
	ctx := payrollCtx()
	org := mustPayrollOrg(t, s, "lineco")
	emp := mustPayrollEmployee(t, s, org.ID, "E-1")
	run := mustPayrollRun(t, s, org.ID, "2026-10")

	line, err := s.UpsertPayLine(ctx, EmployeePayLine{
		OrganizationID: org.ID, RunID: run.ID, EmployeeID: emp.ID,
		GrossMinor: 500000, AdditionsMinor: 500000, DeductionsMinor: 50000, NetMinor: 450000,
		Items: []payroll.PayLineItem{
			{PresetCode: "BASIC", Name: "Basic", Kind: payroll.PresetKind("addition"), AmountMinor: 500000},
		},
		Statutory:   payroll.StatutorySplits{EPFEmployee: 55000},
		BankAccount: "123", BankCode: "BCA",
	})
	if err != nil {
		t.Fatalf("UpsertPayLine insert: %v", err)
	}
	if line.Version != 1 {
		t.Errorf("insert version = %d, want 1", line.Version)
	}

	got, err := s.GetPayLine(ctx, org.ID, run.ID, emp.ID)
	if err != nil {
		t.Fatalf("GetPayLine: %v", err)
	}
	if got.NetMinor != 450000 || got.Statutory.EPFEmployee != 55000 || len(got.Items) != 1 {
		t.Errorf("round trip mismatch: %+v", got)
	}

	// Deep-copy across the boundary: mutating the returned slice must not
	// change stored state.
	got.Items[0].AmountMinor = 1
	gotAgain, err := s.GetPayLine(ctx, org.ID, run.ID, emp.ID)
	if err != nil {
		t.Fatalf("GetPayLine: %v", err)
	}
	if gotAgain.Items[0].AmountMinor != 500000 {
		t.Error("store state mutated through returned slice")
	}

	// Version-checked update.
	got.NetMinor = 460000
	updated, err := s.UpsertPayLine(ctx, got)
	if err != nil {
		t.Fatalf("UpsertPayLine update: %v", err)
	}
	if updated.Version != 2 || updated.NetMinor != 460000 {
		t.Errorf("update result wrong: %+v", updated)
	}
	// Stale version.
	stale := got // version 1
	if _, err := s.UpsertPayLine(ctx, stale); !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale upsert: want ErrVersionConflict, got %v", err)
	}

	// Unknown employee.
	if _, err := s.UpsertPayLine(ctx, EmployeePayLine{
		OrganizationID: org.ID, RunID: run.ID, EmployeeID: "ghost", GrossMinor: 1,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("ghost employee: want ErrNotFound, got %v", err)
	}

	// Locked run rejects upserts.
	if _, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, run.Version, PayrollRunApproved, "me"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := s.UpsertPayLine(ctx, EmployeePayLine{
		OrganizationID: org.ID, RunID: run.ID, EmployeeID: emp.ID, Version: updated.Version,
	}); !errors.Is(err, ErrRunNotDraft) {
		t.Errorf("approved run upsert: want ErrRunNotDraft, got %v", err)
	}

	lines, _, err := s.ListPayLines(ctx, org.ID, run.ID, Page{})
	if err != nil || len(lines) != 1 {
		t.Errorf("ListPayLines: got %d, err %v", len(lines), err)
	}
	if _, err := s.GetPayLine(ctx, org.ID, run.ID, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown pay line: want ErrNotFound, got %v", err)
	}
}

// --- sync items ---

func TestSyncItemsDraftGateAndAtomicity(t *testing.T) {
	s := NewMemoryStore()
	ctx := payrollCtx()
	org := mustPayrollOrg(t, s, "syncco")
	emp := mustPayrollEmployee(t, s, org.ID, "E-1")
	run := mustPayrollRun(t, s, org.ID, "2026-10")

	// Empty is a no-op.
	if err := s.AddSyncItems(ctx, org.ID, run.ID, nil); err != nil {
		t.Fatalf("empty add: %v", err)
	}

	items := []SyncItem{
		{EmployeeID: emp.ID, Source: "attendance", Label: "Overtime", Kind: payroll.PresetKind("addition"), AmountMinor: 10000},
		{EmployeeID: emp.ID, Source: "claims", Label: "Advance", Kind: payroll.PresetKind("deduction"), AmountMinor: 5000},
	}
	if err := s.AddSyncItems(ctx, org.ID, run.ID, items); err != nil {
		t.Fatalf("AddSyncItems: %v", err)
	}
	listed, _, err := s.ListSyncItems(ctx, org.ID, run.ID, Page{})
	if err != nil || len(listed) != 2 {
		t.Fatalf("ListSyncItems: got %d, err %v", len(listed), err)
	}

	// One invalid item aborts the whole add (atomic): nothing is stored.
	bad := []SyncItem{
		{EmployeeID: emp.ID, Kind: payroll.PresetKind("addition"), AmountMinor: 1},
		{EmployeeID: emp.ID, Kind: payroll.PresetKind("bogus"), AmountMinor: 2},
	}
	if err := s.AddSyncItems(ctx, org.ID, run.ID, bad); err == nil {
		t.Fatal("invalid sync item accepted")
	}
	listed, _, err = s.ListSyncItems(ctx, org.ID, run.ID, Page{})
	if err != nil || len(listed) != 2 {
		t.Errorf("atomicity broken: got %d items, err %v", len(listed), err)
	}

	// Unknown employee aborts the whole add too.
	ghost := []SyncItem{{EmployeeID: "ghost", Kind: payroll.PresetKind("addition"), AmountMinor: 1}}
	if err := s.AddSyncItems(ctx, org.ID, run.ID, ghost); !errors.Is(err, ErrNotFound) {
		t.Errorf("ghost employee: want ErrNotFound, got %v", err)
	}
	listed, _, _ = s.ListSyncItems(ctx, org.ID, run.ID, Page{})
	if len(listed) != 2 {
		t.Errorf("atomicity broken on ghost: got %d items", len(listed))
	}

	// Non-draft run rejects.
	if _, err := s.TransitionPayrollRun(ctx, org.ID, run.ID, run.Version, PayrollRunApproved, "me"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := s.AddSyncItems(ctx, org.ID, run.ID, items); !errors.Is(err, ErrRunNotDraft) {
		t.Errorf("approved run add: want ErrRunNotDraft, got %v", err)
	}
	// Unknown run.
	if err := s.AddSyncItems(ctx, org.ID, "nope", items); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown run: want ErrNotFound, got %v", err)
	}
}

// --- previews ---

func TestPayrollPreviewExpiry(t *testing.T) {
	s := NewMemoryStore()
	ctx := payrollCtx()
	org := mustPayrollOrg(t, s, "prevco")
	run := mustPayrollRun(t, s, org.ID, "2026-10")

	saved, err := s.SavePayrollPreview(ctx, PayrollPreview{
		OrganizationID: org.ID, RunID: run.ID,
		Payload:   []byte(`{"total":42}`),
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("SavePayrollPreview: %v", err)
	}
	got, err := s.GetPayrollPreview(ctx, org.ID, saved.ID)
	if err != nil {
		t.Fatalf("GetPayrollPreview: %v", err)
	}
	if string(got.Payload) != `{"total":42}` {
		t.Errorf("payload mismatch: %s", got.Payload)
	}
	// Deep-copy: mutate the returned payload, re-read, must be unchanged.
	got.Payload[0] = 'X'
	gotAgain, _ := s.GetPayrollPreview(ctx, org.ID, saved.ID)
	if string(gotAgain.Payload) != `{"total":42}` {
		t.Error("preview payload mutated through returned value")
	}

	// Expired previews read as not found.
	expired, err := s.SavePayrollPreview(ctx, PayrollPreview{
		OrganizationID: org.ID, RunID: run.ID,
		Payload:   []byte(`{}`),
		ExpiresAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("SavePayrollPreview expired: %v", err)
	}
	if _, err := s.GetPayrollPreview(ctx, org.ID, expired.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired preview: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetPayrollPreview(ctx, org.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown preview: want ErrNotFound, got %v", err)
	}
}

// --- isolation + pagination ---

func TestPayrollCrossOrgIsolation(t *testing.T) {
	s := NewMemoryStore()
	ctx := payrollCtx()
	a := mustPayrollOrg(t, s, "isoa")
	b := mustPayrollOrg(t, s, "isob")
	emp := mustPayrollEmployee(t, s, a.ID, "E-1")
	run := mustPayrollRun(t, s, a.ID, "2026-10")
	if _, err := s.UpsertPayLine(ctx, EmployeePayLine{
		OrganizationID: a.ID, RunID: run.ID, EmployeeID: emp.ID, GrossMinor: 100,
	}); err != nil {
		t.Fatalf("UpsertPayLine: %v", err)
	}
	pr, err := s.CreatePayrollPreset(ctx, a.ID, PayrollItemPreset{
		Code: "A-ONLY", Name: "a", Kind: payroll.PresetKind("addition"),
	})
	if err != nil {
		t.Fatalf("CreatePayrollPreset: %v", err)
	}

	if _, err := s.GetPayrollRun(ctx, b.ID, run.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-org run: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetPayLine(ctx, b.ID, run.ID, emp.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-org pay line: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetPayrollPreset(ctx, b.ID, pr.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-org preset: want ErrNotFound, got %v", err)
	}
	runs, _, err := s.ListPayrollRuns(ctx, b.ID, PayrollRunFilter{}, Page{})
	if err != nil || len(runs) != 0 {
		t.Errorf("cross-org list runs: got %d, err %v", len(runs), err)
	}
	// System presets are visible to every org.
	got, _, err := s.ListPayrollPresets(ctx, b.ID, Page{})
	if err != nil || len(got) != len(payroll.SeedPresets()) {
		t.Errorf("org B should see only seeds: got %d, err %v", len(got), err)
	}
}

func TestPayrollRunPagination(t *testing.T) {
	s := NewMemoryStore()
	ctx := payrollCtx()
	org := mustPayrollOrg(t, s, "pageco")
	periods := []string{"2026-01", "2026-02", "2026-03", "2026-04", "2026-05"}
	for _, per := range periods {
		mustPayrollRun(t, s, org.ID, per)
	}
	var all []PayrollRun
	cursor := ""
	for {
		page, next, err := s.ListPayrollRuns(ctx, org.ID, PayrollRunFilter{}, Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListPayrollRuns: %v", err)
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(all) != len(periods) {
		t.Fatalf("paged through %d runs, want %d", len(all), len(periods))
	}
	seen := map[string]bool{}
	for _, r := range all {
		if seen[r.Period] {
			t.Errorf("duplicate period %s in pagination", r.Period)
		}
		seen[r.Period] = true
	}
}
