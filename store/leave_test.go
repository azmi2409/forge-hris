package store

// Leave domain tests: the pure accrual engine (table tests) plus the
// MemoryStore implementation (CRUD, balance checks, approval chains,
// bulk approval, carry-forward/expiry, pagination).

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- test fixtures ---

func leaveCtx() context.Context { return context.Background() }

func mustLeaveOrg(t *testing.T, s *MemoryStore) Organization {
	t.Helper()
	org, err := s.CreateOrganization(leaveCtx(), Organization{
		Name:        "Leave Co",
		Slug:        "leave-" + strings.ToLower(newID()[:8]),
		CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	return org
}

func mustLeaveEmployee(t *testing.T, s *MemoryStore, orgID, number string, start time.Time) Employee {
	t.Helper()
	emp, err := s.CreateEmployee(leaveCtx(), Employee{
		OrganizationID: orgID,
		EmployeeNumber: number,
		FirstName:      "Test",
		LastName:       number,
		Status:         InvitationJoined,
		WorkStartDate:  &start,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	return emp
}

func mustLeavePolicy(t *testing.T, s *MemoryStore, orgID string, mode AccrualMode, amount, cap float64, expiry int) TimeOffPolicy {
	t.Helper()
	pol, err := s.CreateTimeOffPolicy(leaveCtx(), TimeOffPolicy{
		OrganizationID:  orgID,
		Name:            "Annual-" + string(mode),
		AccrualMode:     mode,
		AccrualAmount:   amount,
		CarryForwardCap: cap,
		ExpiryMonths:    expiry,
	})
	if err != nil {
		t.Fatalf("CreateTimeOffPolicy: %v", err)
	}
	return pol
}

// --- pure accrual engine ---

func TestAccrue(t *testing.T) {
	cases := []struct {
		name   string
		mode   AccrualMode
		amount float64
		months int
		want   float64
	}{
		{"annually prorated", AccrualAnnually, 12, 6, 6},
		{"annually full year", AccrualAnnually, 12, 12, 12},
		{"annually capped at 12 months", AccrualAnnually, 12, 30, 12},
		{"annually zero months", AccrualAnnually, 12, 0, 0},
		{"annually negative months", AccrualAnnually, 12, -3, 0},
		{"monthly", AccrualMonthly, 1.5, 6, 9},
		{"monthly zero", AccrualMonthly, 1.5, 0, 0},
		{"pay period mirrors monthly", AccrualPayPeriod, 2, 3, 6},
		{"payroll hours yields zero without time data", AccrualPayrollHours, 1, 12, 0},
		{"unknown mode yields zero", AccrualMode("bogus"), 5, 12, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Accrue(TimeOffPolicy{AccrualMode: tc.mode, AccrualAmount: tc.amount},
				time.Now(), tc.months)
			if got != tc.want {
				t.Fatalf("Accrue(%s, %v, %d) = %v, want %v",
					tc.mode, tc.amount, tc.months, got, tc.want)
			}
		})
	}
}

func TestApplyCarryForward(t *testing.T) {
	pol := func(cap float64) TimeOffPolicy { return TimeOffPolicy{CarryForwardCap: cap} }
	cases := []struct {
		name string
		prev LeaveBalance
		cap  float64
		want float64
	}{
		{"capped", LeaveBalance{Accrued: 12, Taken: 5}, 5, 5},
		{"under cap", LeaveBalance{Accrued: 12, Taken: 9}, 5, 3},
		{"zero remaining", LeaveBalance{Accrued: 12, Taken: 12}, 5, 0},
		{"negative remaining floors at zero", LeaveBalance{Accrued: 12, Taken: 20}, 5, 0},
		{"cap zero disables carry", LeaveBalance{Accrued: 12, Taken: 5}, 0, 0},
		{"expired reduces carryable", LeaveBalance{Accrued: 10, Carried: 5, Expired: 5, Taken: 4}, 10, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ApplyCarryForward(tc.prev, pol(tc.cap)); got != tc.want {
				t.Fatalf("ApplyCarryForward = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestApplyExpiry(t *testing.T) {
	pol := TimeOffPolicy{ExpiryMonths: 3}
	bal := LeaveBalance{Period: "2026", Accrued: 12, Carried: 5, Taken: 4}
	// Deadline: 2026-12-31 + 3 months = 2027-03-31.
	if got := ApplyExpiry(bal, pol, time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Fatalf("at deadline: got %v, want 0", got)
	}
	if got := ApplyExpiry(bal, pol, time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)); got != 5 {
		t.Fatalf("past deadline: got %v, want 5", got)
	}
	// Capped so the balance never goes negative: available = 2+5-6 = 1.
	tight := LeaveBalance{Period: "2026", Accrued: 2, Carried: 5, Taken: 6}
	if got := ApplyExpiry(tight, pol, time.Date(2027, 6, 1, 0, 0, 0, 0, time.UTC)); got != 1 {
		t.Fatalf("capped expiry: got %v, want 1", got)
	}
	// No expiry configured, or nothing carried: never expires.
	if got := ApplyExpiry(bal, TimeOffPolicy{}, time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Fatalf("no expiry policy: got %v, want 0", got)
	}
	if got := ApplyExpiry(LeaveBalance{Period: "2026", Accrued: 12}, pol,
		time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Fatalf("nothing carried: got %v, want 0", got)
	}
}

func TestWorkingDays(t *testing.T) {
	d := func(s string) time.Time {
		tm, err := time.Parse(dateLayout, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	hol := []Holiday{{Date: "2026-12-25", Name: "Christmas"}}
	cases := []struct {
		name     string
		start    string
		end      string
		holidays []Holiday
		want     float64
	}{
		{"mon-fri week", "2026-11-02", "2026-11-06", nil, 5},
		{"weekend excluded", "2026-11-02", "2026-11-08", nil, 5},
		{"holiday excluded", "2026-12-24", "2026-12-28", hol, 2}, // Thu, Fri-holiday, Mon
		{"weekend only", "2026-11-07", "2026-11-08", nil, 0},
		{"single weekday", "2026-11-04", "2026-11-04", nil, 1},
		{"single weekend day", "2026-11-07", "2026-11-07", nil, 0},
		{"end before start", "2026-11-06", "2026-11-02", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := WorkingDays(d(tc.start), d(tc.end), tc.holidays); got != tc.want {
				t.Fatalf("WorkingDays(%s, %s) = %v, want %v", tc.start, tc.end, got, tc.want)
			}
		})
	}
}

func TestMonthsEmployed(t *testing.T) {
	start := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	emp := Employee{WorkStartDate: &start}
	cases := []struct {
		name string
		asOf time.Time
		want int
	}{
		{"before anniversary day", time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC), 5},
		{"on anniversary day", time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC), 6},
		{"before start", time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MonthsEmployed(emp, tc.asOf); got != tc.want {
				t.Fatalf("MonthsEmployed = %d, want %d", got, tc.want)
			}
		})
	}
	// Falls back to record creation when no work start date is set.
	created := Employee{CreatedAt: time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)}
	if got := MonthsEmployed(created, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)); got != 6 {
		t.Fatalf("fallback to CreatedAt: got %d, want 6", got)
	}
}

func TestRecalculateBalanceIdempotent(t *testing.T) {
	pol := TimeOffPolicy{ID: "p1", OrganizationID: "o1", AccrualMode: AccrualMonthly, AccrualAmount: 1.5}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	emp := Employee{ID: "e1", OrganizationID: "o1", WorkStartDate: &start}
	asOf := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	reqs := []TimeOffRequest{
		{ID: "r1", PolicyID: "p1", EmployeeID: "e1", StartDate: "2026-03-02", EndDate: "2026-03-06", Status: TimeOffRequested},
		{ID: "r2", PolicyID: "p1", EmployeeID: "e1", StartDate: "2026-04-01", EndDate: "2026-04-03", Status: TimeOffApproved},
		{ID: "r3", PolicyID: "p1", EmployeeID: "e1", StartDate: "2026-05-01", EndDate: "2026-05-05", Status: TimeOffRejected},
		{ID: "r4", PolicyID: "p1", EmployeeID: "e1", StartDate: "2026-05-11", EndDate: "2026-05-15", Status: TimeOffApproved, IsUnpaid: true},
		{ID: "r5", PolicyID: "other", EmployeeID: "e1", StartDate: "2026-03-02", EndDate: "2026-03-06", Status: TimeOffApproved},
	}
	b1 := RecalculateBalance(pol, emp, reqs, nil, asOf)
	b2 := RecalculateBalance(pol, emp, reqs, nil, asOf)
	if !reflect.DeepEqual(b1, b2) {
		t.Fatalf("not idempotent:\n%+v\n%+v", b1, b2)
	}
	// 6 months x 1.5 = 9 accrued; taken = 5 (r1) + 3 (r2); r3 rejected,
	// r4 unpaid, r5 other policy are excluded.
	if b1.Period != "2026" || b1.Accrued != 9 || b1.Taken != 8 || b1.Balance != 1 {
		t.Fatalf("unexpected balance: %+v", b1)
	}
	if b1.Carried != 0 || b1.Expired != 0 {
		t.Fatalf("pure recalc must yield carried=0 expired=0: %+v", b1)
	}
	if !b1.ComputedAt.Equal(asOf.UTC()) {
		t.Fatalf("ComputedAt should be asOf for idempotency, got %v", b1.ComputedAt)
	}
}

// --- MemoryStore: policies ---

func TestLeavePolicyCRUD(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)

	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnual(), 12, 5, 3)
	if pol.Version != 1 {
		t.Fatalf("unexpected created policy: %+v", pol)
	}
	got, err := s.GetTimeOffPolicy(ctx, org.ID, pol.ID)
	if err != nil || got.Name != pol.Name {
		t.Fatalf("GetTimeOffPolicy: %+v %v", got, err)
	}
	if _, err := s.GetTimeOffPolicy(ctx, org.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing policy: want ErrNotFound, got %v", err)
	}

	pol.Name = "Renamed"
	updated, err := s.UpdateTimeOffPolicy(ctx, pol)
	if err != nil || updated.Version != 2 || updated.Name != "Renamed" {
		t.Fatalf("UpdateTimeOffPolicy: %+v %v", updated, err)
	}
	pol.Name = "Stale"
	if _, err := s.UpdateTimeOffPolicy(ctx, pol); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: want ErrVersionConflict, got %v", err)
	}

	if err := s.DeleteTimeOffPolicy(ctx, org.ID, pol.ID); err != nil {
		t.Fatalf("DeleteTimeOffPolicy: %v", err)
	}
	if _, err := s.GetTimeOffPolicy(ctx, org.ID, pol.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: want ErrNotFound, got %v", err)
	}
	if err := s.DeleteTimeOffPolicy(ctx, org.ID, pol.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

func AccrualAnnual() AccrualMode { return AccrualAnnually }

func TestDeletePolicyWithRequestsIsNotEmpty(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 12, 0, 0)
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-11-02", EndDate: "2026-11-06",
	}, nil); err != nil {
		t.Fatalf("CreateTimeOffRequest: %v", err)
	}
	if err := s.DeleteTimeOffPolicy(ctx, org.ID, pol.ID); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("delete with requests: want ErrNotEmpty, got %v", err)
	}
}

// --- MemoryStore: requests and balances ---

func TestCreateRequestComputesDaysAndSnapshot(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 12, 0, 0)
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	req, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-11-02", EndDate: "2026-11-06", // Mon-Fri
	}, nil)
	if err != nil {
		t.Fatalf("CreateTimeOffRequest: %v", err)
	}
	if req.Days != 5 {
		t.Fatalf("Days = %v, want 5", req.Days)
	}
	if req.Status != TimeOffRequested || req.Version != 1 {
		t.Fatalf("unexpected request state: %+v", req)
	}
	if len(req.Snapshot) == 0 || !strings.Contains(string(req.Snapshot), `"balance_at_request"`) {
		t.Fatalf("snapshot missing: %s", req.Snapshot)
	}
	if len(req.ApproverIDs) != 0 {
		t.Fatalf("ApproverIDs should not persist on the request: %v", req.ApproverIDs)
	}
}

func TestCreateRequestInsufficientBalance(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 12, 0, 0)
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	if _, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-11-02", EndDate: "2026-11-06", // 5 days of 12
	}, nil); err != nil {
		t.Fatalf("first request: %v", err)
	}
	_, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-12-01", EndDate: "2026-12-11", // 9 days; only 7 left
	}, nil)
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("over-balance request: want ErrInsufficientBalance, got %v", err)
	}
	// Unpaid requests skip the balance check entirely.
	unpaid, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-12-01", EndDate: "2026-12-31", IsUnpaid: true, // 23 working days
	}, nil)
	if err != nil {
		t.Fatalf("unpaid request: %v", err)
	}
	if !unpaid.IsUnpaid || unpaid.Days != 23 {
		t.Fatalf("unexpected unpaid request: %+v", unpaid)
	}
}

func TestCreateRequestNoWorkingDaysRejected(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 12, 0, 0)
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	_, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-11-07", EndDate: "2026-11-08", // Sat-Sun
	}, nil)
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("weekend-only request: want ValidationError, got %v", err)
	}
}

func TestRecalculateLeaveBalanceCarryForward(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualMonthly, 1, 5, 0) // 1 day/month, cap 5
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	// Seed the 2025 balance: 17 months tenure x 1 day/month, take 4 in Dec 2025.
	asOf2025 := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	b2025, err := s.RecalculateLeaveBalance(ctx, org.ID, emp.ID, pol.ID, asOf2025, nil)
	if err != nil {
		t.Fatalf("recalc 2025: %v", err)
	}
	if b2025.Period != "2025" || b2025.Accrued != 17 || b2025.Balance != 17 {
		t.Fatalf("unexpected 2025 balance: %+v", b2025)
	}
	if _, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2025-12-01", EndDate: "2025-12-04", // Tue-Fri = 4 days
	}, nil); err != nil {
		t.Fatalf("2025 request: %v", err)
	}
	b2025, err = s.RecalculateLeaveBalance(ctx, org.ID, emp.ID, pol.ID, asOf2025, nil)
	if err != nil {
		t.Fatalf("recalc 2025 again: %v", err)
	}
	if b2025.Taken != 4 || b2025.Balance != 13 {
		t.Fatalf("2025 after leave: %+v", b2025)
	}
	// 2026 recalc picks up carried = min(17-4, cap 5) = 5; accrued = 26.
	b2026, err := s.RecalculateLeaveBalance(ctx, org.ID, emp.ID, pol.ID,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("recalc 2026: %v", err)
	}
	if b2026.Period != "2026" || b2026.Accrued != 26 || b2026.Carried != 5 || b2026.Balance != 31 {
		t.Fatalf("2026 with carry: %+v", b2026)
	}
	// Idempotent: recalculating again yields the same stored balance.
	again, err := s.RecalculateLeaveBalance(ctx, org.ID, emp.ID, pol.ID,
		time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatalf("recalc 2026 again: %v", err)
	}
	if again.Balance != b2026.Balance || again.Carried != b2026.Carried || again.Version != b2026.Version+1 {
		t.Fatalf("recalc not stable: %+v vs %+v", again, b2026)
	}
}

// --- MemoryStore: approval chains ---

func leaveChainFixture(t *testing.T, s *MemoryStore) (orgID, empID, mgrID, hrID, polID, reqID string) {
	t.Helper()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 20, 0, 0)
	mgr := mustLeaveEmployee(t, s, org.ID, "MGR", time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	hr := mustLeaveEmployee(t, s, org.ID, "HR", time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	req, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-11-02", EndDate: "2026-11-06",
		ApproverIDs: []string{mgr.ID, hr.ID},
	}, nil)
	if err != nil {
		t.Fatalf("CreateTimeOffRequest: %v", err)
	}
	return org.ID, emp.ID, mgr.ID, hr.ID, pol.ID, req.ID
}

func TestApprovalChainAdvancement(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	orgID, _, mgrID, hrID, _, reqID := leaveChainFixture(t, s)

	steps, _, err := s.ListTimeOffApprovals(ctx, orgID, reqID, Page{})
	if err != nil || len(steps) != 2 {
		t.Fatalf("chain: %+v %v", steps, err)
	}
	if steps[0].Sequence != 1 || steps[0].ApproverEmployeeID != mgrID ||
		steps[1].Sequence != 2 || steps[1].ApproverEmployeeID != hrID {
		t.Fatalf("chain order wrong: %+v", steps)
	}

	// Out of order: HR cannot decide before the manager.
	req, _ := s.GetTimeOffRequest(ctx, orgID, reqID)
	if _, _, err := s.DecideTimeOffApproval(ctx, orgID, reqID, hrID, true, req.Version, false); !errors.Is(err, ErrNotCurrentApprover) {
		t.Fatalf("out-of-order decide: want ErrNotCurrentApprover, got %v", err)
	}
	// Manager approves: chain advances, request stays requested.
	updated, chain, err := s.DecideTimeOffApproval(ctx, orgID, reqID, mgrID, true, req.Version, false)
	if err != nil {
		t.Fatalf("manager approve: %v", err)
	}
	if updated.Status != TimeOffRequested || updated.Version != req.Version+1 {
		t.Fatalf("after first approval: %+v", updated)
	}
	if chain[0].Status != ApprovalApproved || chain[1].Status != ApprovalPending {
		t.Fatalf("chain states wrong: %+v", chain)
	}
	// HR approves: no pending steps remain -> approved.
	updated2, _, err := s.DecideTimeOffApproval(ctx, orgID, reqID, hrID, true, updated.Version, false)
	if err != nil {
		t.Fatalf("hr approve: %v", err)
	}
	if updated2.Status != TimeOffApproved {
		t.Fatalf("after final approval: %+v", updated2)
	}
	// Deciding an approved request is an invalid transition.
	if _, _, err := s.DecideTimeOffApproval(ctx, orgID, reqID, hrID, true, updated2.Version, false); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("decide approved: want ErrInvalidTransition, got %v", err)
	}
}

func TestRejectShortCircuitsChain(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	orgID, _, mgrID, hrID, _, reqID := leaveChainFixture(t, s)

	req, _ := s.GetTimeOffRequest(ctx, orgID, reqID)
	updated, chain, err := s.DecideTimeOffApproval(ctx, orgID, reqID, mgrID, false, req.Version, false)
	if err != nil {
		t.Fatalf("manager reject: %v", err)
	}
	if updated.Status != TimeOffRejected {
		t.Fatalf("after reject: %+v", updated)
	}
	if chain[0].Status != ApprovalRejected || chain[1].Status != ApprovalPending {
		t.Fatalf("chain after reject: %+v", chain)
	}
	// The rest of the chain can no longer act.
	if _, _, err := s.DecideTimeOffApproval(ctx, orgID, reqID, hrID, true, updated.Version, false); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("decide rejected: want ErrInvalidTransition, got %v", err)
	}
}

func TestDecideVersionConflict(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	orgID, _, mgrID, _, _, reqID := leaveChainFixture(t, s)
	if _, _, err := s.DecideTimeOffApproval(ctx, orgID, reqID, mgrID, true, 999, false); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale decide: want ErrVersionConflict, got %v", err)
	}
}

func TestAdminDecidesChainlessRequest(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 20, 0, 0)
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	req, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-11-02", EndDate: "2026-11-06",
	}, nil) // no approvers: chainless
	if err != nil {
		t.Fatalf("CreateTimeOffRequest: %v", err)
	}
	// A non-admin cannot decide a chainless request.
	if _, _, err := s.DecideTimeOffApproval(ctx, org.ID, req.ID, emp.ID, true, req.Version, false); !errors.Is(err, ErrNotCurrentApprover) {
		t.Fatalf("non-admin chainless decide: want ErrNotCurrentApprover, got %v", err)
	}
	updated, _, err := s.DecideTimeOffApproval(ctx, org.ID, req.ID, emp.ID, true, req.Version, true)
	if err != nil {
		t.Fatalf("admin decide: %v", err)
	}
	if updated.Status != TimeOffApproved {
		t.Fatalf("admin-approved chainless request: %+v", updated)
	}
}

func TestCancelTransitions(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 30, 0, 0)
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	mk := func(start, end string) TimeOffRequest {
		t.Helper()
		req, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
			OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
			StartDate: start, EndDate: end,
		}, nil)
		if err != nil {
			t.Fatalf("CreateTimeOffRequest: %v", err)
		}
		return req
	}

	// Requested -> cancelled.
	r1 := mk("2026-11-02", "2026-11-03")
	c1, err := s.CancelTimeOffRequest(ctx, org.ID, r1.ID, r1.Version)
	if err != nil || c1.Status != TimeOffCancelled {
		t.Fatalf("cancel requested: %+v %v", c1, err)
	}
	// Cancelled is terminal.
	if _, err := s.CancelTimeOffRequest(ctx, org.ID, r1.ID, c1.Version); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancel cancelled: want ErrInvalidTransition, got %v", err)
	}
	// Approved -> cancelled (admin decide with empty chain).
	r2 := mk("2026-11-09", "2026-11-10")
	a2, _, err := s.DecideTimeOffApproval(ctx, org.ID, r2.ID, emp.ID, true, r2.Version, true)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	c2, err := s.CancelTimeOffRequest(ctx, org.ID, r2.ID, a2.Version)
	if err != nil || c2.Status != TimeOffCancelled {
		t.Fatalf("cancel approved: %+v %v", c2, err)
	}
	// Rejected -> cancelled is invalid.
	r3 := mk("2026-11-16", "2026-11-17")
	j3, _, err := s.DecideTimeOffApproval(ctx, org.ID, r3.ID, emp.ID, false, r3.Version, true)
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := s.CancelTimeOffRequest(ctx, org.ID, r3.ID, j3.Version); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("cancel rejected: want ErrInvalidTransition, got %v", err)
	}
	// Stale version.
	if _, err := s.CancelTimeOffRequest(ctx, org.ID, r3.ID, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale cancel: want ErrVersionConflict, got %v", err)
	}
}

func TestBulkApproveAllOrNothing(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	orgID, _, mgrID, _, polID, reqID := leaveChainFixture(t, s)
	// reqID's chain is [mgr, hr]: a bulk approve by mgr approves only the
	// first step, so the request stays requested.
	approved, err := s.BulkApproveTimeOffRequests(ctx, orgID, []string{reqID}, mgrID, false)
	if err != nil {
		t.Fatalf("bulk approve: %v", err)
	}
	if len(approved) != 1 || approved[0].Status != TimeOffRequested {
		t.Fatalf("bulk approved: %+v", approved)
	}

	// Second request, approvable fully by an admin.
	emp := mustLeaveEmployee(t, s, orgID, "E-9", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	r2, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: orgID, EmployeeID: emp.ID, PolicyID: polID,
		StartDate: "2026-12-01", EndDate: "2026-12-02",
	}, nil)
	if err != nil {
		t.Fatalf("CreateTimeOffRequest r2: %v", err)
	}
	// Bulk with one bad id: nothing is applied.
	_, err = s.BulkApproveTimeOffRequests(ctx, orgID, []string{r2.ID, "missing"}, "admin-x", true)
	var berr *BulkApprovalError
	if !errors.As(err, &berr) {
		t.Fatalf("bulk with bad id: want *BulkApprovalError, got %v", err)
	}
	// The valid request validates (Approved=true) but nothing is applied:
	// all-or-nothing.
	if len(berr.Results) != 2 || !berr.Results[0].Approved ||
		berr.Results[1].Approved || berr.Results[1].Reason != "not found" {
		t.Fatalf("bulk results wrong: %+v", berr.Results)
	}
	still, _ := s.GetTimeOffRequest(ctx, orgID, r2.ID)
	if still.Status != TimeOffRequested {
		t.Fatalf("failed bulk applied anyway: %+v", still)
	}
	// Clean bulk by admin approves the chainless request.
	done, err := s.BulkApproveTimeOffRequests(ctx, orgID, []string{r2.ID}, "admin-x", true)
	if err != nil {
		t.Fatalf("admin bulk: %v", err)
	}
	if done[0].Status != TimeOffApproved {
		t.Fatalf("admin bulk result: %+v", done[0])
	}
}

// --- MemoryStore: calendars, holidays, who-is-on-leave, pagination ---

func TestHolidayCalendarCRUD(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)

	cal, err := s.CreateHolidayCalendar(ctx, HolidayCalendar{
		OrganizationID: org.ID, Name: "MY Holidays", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateHolidayCalendar: %v", err)
	}
	if _, err := s.CreateHolidayCalendar(ctx, HolidayCalendar{
		OrganizationID: org.ID, Name: "Bad", CountryCode: "XX",
	}); err == nil {
		t.Fatalf("bad country code accepted")
	}

	h, err := s.CreateHoliday(ctx, Holiday{
		OrganizationID: org.ID, CalendarID: cal.ID, Date: "2026-01-01", Name: "New Year",
	})
	if err != nil {
		t.Fatalf("CreateHoliday: %v", err)
	}
	if _, err := s.CreateHoliday(ctx, Holiday{
		OrganizationID: org.ID, CalendarID: cal.ID, Date: "2026-01-01", Name: "Dupe",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate holiday date: want ErrAlreadyExists, got %v", err)
	}
	if _, err := s.CreateHoliday(ctx, Holiday{
		OrganizationID: org.ID, CalendarID: cal.ID, Date: "not-a-date", Name: "Bad",
	}); err == nil {
		t.Fatalf("bad holiday date accepted")
	}

	list, _, err := s.ListHolidays(ctx, org.ID, cal.ID, Page{})
	if err != nil || len(list) != 1 || list[0].ID != h.ID {
		t.Fatalf("ListHolidays: %+v %v", list, err)
	}
	if err := s.DeleteHolidayCalendar(ctx, org.ID, cal.ID); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("delete calendar with holidays: want ErrNotEmpty, got %v", err)
	}
	if err := s.DeleteHoliday(ctx, org.ID, h.ID); err != nil {
		t.Fatalf("DeleteHoliday: %v", err)
	}
	if err := s.DeleteHoliday(ctx, org.ID, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete holiday: want ErrNotFound, got %v", err)
	}
	if err := s.DeleteHolidayCalendar(ctx, org.ID, cal.ID); err != nil {
		t.Fatalf("DeleteHolidayCalendar: %v", err)
	}
}

func TestListApprovedLeaveOnDate(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 30, 0, 0)
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))

	mk := func(start, end string) TimeOffRequest {
		t.Helper()
		req, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
			OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
			StartDate: start, EndDate: end,
		}, nil)
		if err != nil {
			t.Fatalf("CreateTimeOffRequest: %v", err)
		}
		return req
	}
	onLeave := mk("2026-10-15", "2026-10-17") // Thu-Sat
	pending := mk("2026-10-15", "2026-10-16")

	for _, req := range []TimeOffRequest{onLeave} {
		if _, _, err := s.DecideTimeOffApproval(ctx, org.ID, req.ID, emp.ID, true, req.Version, true); err != nil {
			t.Fatalf("approve: %v", err)
		}
	}
	_ = pending

	got, _, err := s.ListApprovedLeaveOnDate(ctx, org.ID, "2026-10-16", Page{})
	if err != nil {
		t.Fatalf("ListApprovedLeaveOnDate: %v", err)
	}
	if len(got) != 1 || got[0].ID != onLeave.ID {
		t.Fatalf("who is on leave 2026-10-16: %+v", got)
	}
	got, _, err = s.ListApprovedLeaveOnDate(ctx, org.ID, "2026-10-19", Page{})
	if err != nil || len(got) != 0 {
		t.Fatalf("nobody on leave 2026-10-19: %+v %v", got, err)
	}
	if _, _, err := s.ListApprovedLeaveOnDate(ctx, org.ID, "bogus", Page{}); err == nil {
		t.Fatalf("bad date accepted")
	}
}

func TestLeavePaginationWalk(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	for i := 0; i < 3; i++ {
		if _, err := s.CreateTimeOffPolicy(ctx, TimeOffPolicy{
			OrganizationID: org.ID, Name: "P", AccrualMode: AccrualMonthly, AccrualAmount: 1,
		}); err != nil {
			t.Fatalf("CreateTimeOffPolicy: %v", err)
		}
	}
	var seen []string
	cursor := ""
	for {
		page, next, err := s.ListTimeOffPolicies(ctx, org.ID, Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("ListTimeOffPolicies: %v", err)
		}
		for _, p := range page {
			seen = append(seen, p.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Fatalf("pagination walk saw %d policies, want 3", len(seen))
	}
	if _, _, err := s.ListTimeOffPolicies(ctx, org.ID, Page{Cursor: "bogus"}); err == nil {
		t.Fatalf("bad cursor accepted")
	}
}

func TestUpdateRequestValidation(t *testing.T) {
	s := NewMemoryStore()
	ctx := leaveCtx()
	org := mustLeaveOrg(t, s)
	pol := mustLeavePolicy(t, s, org.ID, AccrualAnnually, 30, 0, 0)
	emp := mustLeaveEmployee(t, s, org.ID, "E-1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	req, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		StartDate: "2026-11-02", EndDate: "2026-11-06",
	}, nil)
	if err != nil {
		t.Fatalf("CreateTimeOffRequest: %v", err)
	}
	req.EndDate = "2026-11-09" // extend through the following Monday: 6 working days
	updated, err := s.UpdateTimeOffRequest(ctx, req, nil)
	if err != nil {
		t.Fatalf("UpdateTimeOffRequest: %v", err)
	}
	if updated.Days != 6 || updated.Version != 2 {
		t.Fatalf("updated request: %+v", updated)
	}
	// Approved requests cannot be edited.
	appr, _, err := s.DecideTimeOffApproval(ctx, org.ID, updated.ID, emp.ID, true, updated.Version, true)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	appr.EndDate = "2026-11-10"
	if _, err := s.UpdateTimeOffRequest(ctx, appr, nil); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("edit approved: want ErrInvalidTransition, got %v", err)
	}
}
