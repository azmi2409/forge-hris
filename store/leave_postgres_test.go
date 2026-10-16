package store

// Postgres adapter tests for the leave domain. They run in CI against a
// postgres:16 service and skip cleanly without TEST_DATABASE_URL, reusing
// the testPostgresHRIS helper from postgres_test.go (same package).

import (
	"errors"
	"testing"
	"time"
)

// TestPostgresLeaveRoundTrip exercises policies, calendars, holidays,
// requests with an approval chain, decisions, and balance recalculation
// through the pgx adapter.
func TestPostgresLeaveRoundTrip(t *testing.T) {
	s, ctx := testPostgresHRIS(t)

	org, err := s.CreateOrganization(ctx, Organization{
		Name: "Leave PG", Slug: "leave-pg", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	mgr, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID, EmployeeNumber: "MGR", FirstName: "Mgr",
		Status: InvitationJoined, WorkStartDate: &start,
	})
	if err != nil {
		t.Fatalf("CreateEmployee mgr: %v", err)
	}
	emp, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID, EmployeeNumber: "E-1", FirstName: "Emp",
		Status: InvitationJoined, WorkStartDate: &start, ManagerID: &mgr.ID,
	})
	if err != nil {
		t.Fatalf("CreateEmployee emp: %v", err)
	}
	pol, err := s.CreateTimeOffPolicy(ctx, TimeOffPolicy{
		OrganizationID: org.ID, Name: "Annual", AccrualMode: AccrualAnnually,
		AccrualAmount: 12,
	})
	if err != nil {
		t.Fatalf("CreateTimeOffPolicy: %v", err)
	}
	// Version-checked update through the adapter.
	pol.Name = "Annual Leave"
	pol, err = s.UpdateTimeOffPolicy(ctx, pol)
	if err != nil || pol.Version != 2 {
		t.Fatalf("UpdateTimeOffPolicy: %+v %v", pol, err)
	}

	cal, err := s.CreateHolidayCalendar(ctx, HolidayCalendar{
		OrganizationID: org.ID, Name: "MY", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateHolidayCalendar: %v", err)
	}
	hol, err := s.CreateHoliday(ctx, Holiday{
		OrganizationID: org.ID, CalendarID: cal.ID, Date: "2026-12-25", Name: "Christmas",
	})
	if err != nil {
		t.Fatalf("CreateHoliday: %v", err)
	}
	if _, err := s.CreateHoliday(ctx, Holiday{
		OrganizationID: org.ID, CalendarID: cal.ID, Date: "2026-12-25", Name: "Dupe",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate holiday: want ErrAlreadyExists, got %v", err)
	}
	holidays, _, err := s.ListHolidays(ctx, org.ID, cal.ID, Page{})
	if err != nil || len(holidays) != 1 || holidays[0].ID != hol.ID {
		t.Fatalf("ListHolidays: %+v %v", holidays, err)
	}

	// 2026-12-24..2026-12-28: Thu, Fri-holiday, Mon = 2 working days.
	req, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
		OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
		HolidayCalendarID: &cal.ID,
		StartDate:         "2026-12-24", EndDate: "2026-12-28",
		ApproverIDs: []string{mgr.ID},
	}, holidays)
	if err != nil {
		t.Fatalf("CreateTimeOffRequest: %v", err)
	}
	if req.Days != 2 {
		t.Fatalf("Days = %v, want 2 (holiday excluded)", req.Days)
	}
	if len(req.Snapshot) == 0 {
		t.Fatalf("snapshot missing")
	}
	steps, _, err := s.ListTimeOffApprovals(ctx, org.ID, req.ID, Page{})
	if err != nil || len(steps) != 1 || steps[0].ApproverEmployeeID != mgr.ID {
		t.Fatalf("chain: %+v %v", steps, err)
	}
	decided, chain, err := s.DecideTimeOffApproval(ctx, org.ID, req.ID, mgr.ID, true, req.Version, false)
	if err != nil {
		t.Fatalf("DecideTimeOffApproval: %v", err)
	}
	if decided.Status != TimeOffApproved || chain[0].Status != ApprovalApproved {
		t.Fatalf("after decide: %+v %+v", decided, chain)
	}

	bal, err := s.RecalculateLeaveBalance(ctx, org.ID, emp.ID, pol.ID,
		time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), holidays)
	if err != nil {
		t.Fatalf("RecalculateLeaveBalance: %v", err)
	}
	// Annual 12 days, 12+ months tenure -> 12 accrued; 2 taken.
	if bal.Period != "2026" || bal.Accrued != 12 || bal.Taken != 2 || bal.Balance != 10 {
		t.Fatalf("balance: %+v", bal)
	}
	got, err := s.GetLeaveBalance(ctx, org.ID, emp.ID, pol.ID, "2026")
	if err != nil || got.Balance != 10 {
		t.Fatalf("GetLeaveBalance: %+v %v", got, err)
	}

	onLeave, _, err := s.ListApprovedLeaveOnDate(ctx, org.ID, "2026-12-24", Page{})
	if err != nil || len(onLeave) != 1 || onLeave[0].ID != req.ID {
		t.Fatalf("ListApprovedLeaveOnDate: %+v %v", onLeave, err)
	}

	// Policy deletion is blocked while requests reference it.
	if err := s.DeleteTimeOffPolicy(ctx, org.ID, pol.ID); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("delete referenced policy: want ErrNotEmpty, got %v", err)
	}
}

// TestPostgresLeaveInsufficientBalance checks the balance gate through the
// pgx adapter.
func TestPostgresLeaveInsufficientBalance(t *testing.T) {
	s, ctx := testPostgresHRIS(t)

	org, err := s.CreateOrganization(ctx, Organization{
		Name: "Leave PG2", Slug: "leave-pg2", CountryCode: "ID",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	emp, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID, EmployeeNumber: "E-1", FirstName: "Emp",
		Status: InvitationJoined, WorkStartDate: &start,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	pol, err := s.CreateTimeOffPolicy(ctx, TimeOffPolicy{
		OrganizationID: org.ID, Name: "Annual", AccrualMode: AccrualAnnually,
		AccrualAmount: 12,
	})
	if err != nil {
		t.Fatalf("CreateTimeOffPolicy: %v", err)
	}
	mk := func(startDate, endDate string) error {
		_, err := s.CreateTimeOffRequest(ctx, TimeOffRequest{
			OrganizationID: org.ID, EmployeeID: emp.ID, PolicyID: pol.ID,
			StartDate: startDate, EndDate: endDate,
		}, nil)
		return err
	}
	if err := mk("2026-11-02", "2026-11-06"); err != nil { // 5 of 12
		t.Fatalf("first request: %v", err)
	}
	if err := mk("2026-12-01", "2026-12-11"); !errors.Is(err, ErrInsufficientBalance) { // 9 > 7 left
		t.Fatalf("over-balance: want ErrInsufficientBalance, got %v", err)
	}
}
