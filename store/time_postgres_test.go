package store

// Postgres adapter tests for component 2 (time tracking). They run in CI
// against a postgres:16 service; without TEST_DATABASE_URL they skip
// cleanly via testPostgresHRIS (same helper as postgres_test.go).

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresTimeRoundTrip(t *testing.T) {
	s, ctx := testPostgresHRIS(t)

	org, err := s.CreateOrganization(ctx, Organization{
		Name: "Time PG", Slug: "time-pg", CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	emp, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID, EmployeeNumber: "E-T1", FirstName: "Punch",
		Status: InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}

	// Projects.
	proj, err := s.CreateProject(ctx, Project{OrganizationID: org.ID, Name: "Site", ClientName: "Acme"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := s.GetProject(ctx, org.ID, proj.ID); err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	// Clock flow.
	in, err := s.ClockIn(ctx, org.ID, emp.ID, TimeEntry{
		OccurredAt: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC),
		ProjectID:  &proj.ID,
		ClientType: TimeEntryClientAIAssistant,
	})
	if err != nil {
		t.Fatalf("ClockIn: %v", err)
	}
	if _, err := s.ClockIn(ctx, org.ID, emp.ID, TimeEntry{}); !errors.Is(err, ErrAlreadyClockedIn) {
		t.Fatalf("double clock-in: want ErrAlreadyClockedIn, got %v", err)
	}
	open, isOpen, err := s.OpenTimeEntry(ctx, org.ID, emp.ID)
	if err != nil || !isOpen || open.ID != in.ID {
		t.Fatalf("OpenTimeEntry: %v %v %v", open.ID, isOpen, err)
	}
	out, err := s.ClockOut(ctx, org.ID, emp.ID, TimeEntry{
		OccurredAt: time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ClockOut: %v", err)
	}
	if out.Kind != TimeEntryOut {
		t.Fatalf("kind = %s", out.Kind)
	}

	// Correction chain.
	corr, err := s.CorrectTimeEntry(ctx, org.ID, in.ID, TimeEntry{
		Kind:       TimeEntryIn,
		OccurredAt: time.Date(2026, 10, 12, 8, 55, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CorrectTimeEntry: %v", err)
	}
	if corr.PrevID == nil || *corr.PrevID != in.ID {
		t.Fatalf("correction not linked")
	}
	head, _, err := s.ListTimeEntries(ctx, org.ID, TimeEntryFilter{EmployeeID: emp.ID}, Page{})
	if err != nil {
		t.Fatalf("ListTimeEntries: %v", err)
	}
	if len(head) != 2 { // corrected in + out
		t.Fatalf("want 2 head entries, got %d", len(head))
	}

	// Hour entries.
	he, err := s.CreateHourEntry(ctx, HourEntry{
		OrganizationID: org.ID, EmployeeID: emp.ID,
		WorkDate: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC),
		Hours:    4, Note: "docs",
	})
	if err != nil {
		t.Fatalf("CreateHourEntry: %v", err)
	}
	cur, err := s.GetHourEntry(ctx, org.ID, he.ID)
	if err != nil {
		t.Fatalf("GetHourEntry: %v", err)
	}
	cur.Hours = 5
	if _, err := s.UpdateHourEntry(ctx, cur); err != nil {
		t.Fatalf("UpdateHourEntry: %v", err)
	}

	// Pay period lifecycle + entry freezing.
	def, err := s.CreatePayPeriodDefinition(ctx, PayPeriodDefinition{
		OrganizationID: org.ID, Name: "Monthly", Cadence: PayPeriodCadenceMonthly, AnchorDay: 1,
	})
	if err != nil {
		t.Fatalf("CreatePayPeriodDefinition: %v", err)
	}
	period, err := s.CreatePayPeriod(ctx, org.ID, def.ID, time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("CreatePayPeriod: %v", err)
	}
	if got := period.PeriodStart.Format("2006-01-02"); got != "2026-10-01" {
		t.Fatalf("period start = %s", got)
	}
	if _, err := s.ApprovePayPeriod(ctx, org.ID, period.ID, period.Version); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approve from Open: want ErrInvalidTransition, got %v", err)
	}
	submitted, err := s.SubmitPayPeriod(ctx, org.ID, period.ID, period.Version)
	if err != nil {
		t.Fatalf("SubmitPayPeriod: %v", err)
	}
	approved, err := s.ApprovePayPeriod(ctx, org.ID, period.ID, submitted.Version)
	if err != nil {
		t.Fatalf("ApprovePayPeriod: %v", err)
	}
	if approved.Status != PayPeriodApproved {
		t.Fatalf("status = %s", approved.Status)
	}
	locked, err := s.GetTimeEntry(ctx, org.ID, out.ID)
	if err != nil {
		t.Fatalf("GetTimeEntry: %v", err)
	}
	if !locked.IsLocked {
		t.Fatalf("approval did not lock covered entries")
	}
	upd := locked
	upd.Version = locked.Version
	if _, err := s.UpdateTimeEntry(ctx, upd); !errors.Is(err, ErrPeriodLocked) {
		t.Fatalf("update locked: want ErrPeriodLocked, got %v", err)
	}

	// Schedules and shifts.
	sc, err := s.CreateSchedule(ctx, Schedule{
		OrganizationID: org.ID, EmployeeID: emp.ID, Kind: ScheduleFixed, ExpectedDailyHours: 8,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	scheds, _, err := s.ListSchedules(ctx, org.ID, emp.ID, Page{})
	if err != nil || len(scheds) != 1 || scheds[0].ID != sc.ID {
		t.Fatalf("ListSchedules: %v %d", err, len(scheds))
	}
	sh, err := s.CreateShift(ctx, Shift{
		OrganizationID: org.ID, EmployeeID: emp.ID,
		ShiftDate: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		StartsAt:  time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC),
		EndsAt:    time.Date(2026, 10, 19, 17, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CreateShift: %v", err)
	}
	n, err := s.PublishShifts(ctx, org.ID, []string{sh.ID})
	if err != nil || n != 1 {
		t.Fatalf("PublishShifts: %v %d", err, n)
	}
	got, err := s.GetShift(ctx, org.ID, sh.ID)
	if err != nil || !got.Published {
		t.Fatalf("GetShift published: %v %+v", err, got)
	}
}

func TestPostgresTimeTenantIsolation(t *testing.T) {
	s, ctx := testPostgresHRIS(t)

	orgA, err := s.CreateOrganization(ctx, Organization{Name: "TA", Slug: "time-iso-a", CountryCode: "MY"})
	if err != nil {
		t.Fatalf("CreateOrganization A: %v", err)
	}
	orgB, err := s.CreateOrganization(ctx, Organization{Name: "TB", Slug: "time-iso-b", CountryCode: "ID"})
	if err != nil {
		t.Fatalf("CreateOrganization B: %v", err)
	}
	empB, err := s.CreateEmployee(ctx, Employee{
		OrganizationID: orgB.ID, EmployeeNumber: "E-B", FirstName: "Bee", Status: InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee B: %v", err)
	}
	entryB, err := s.ClockIn(ctx, orgB.ID, empB.ID, TimeEntry{})
	if err != nil {
		t.Fatalf("ClockIn B: %v", err)
	}
	projB, err := s.CreateProject(ctx, Project{OrganizationID: orgB.ID, Name: "Secret"})
	if err != nil {
		t.Fatalf("CreateProject B: %v", err)
	}

	// Adapter-level isolation: org A's reads must not see org B's rows.
	if _, err := s.GetTimeEntry(ctx, orgA.ID, entryB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTimeEntry cross-org: want ErrNotFound, got %v", err)
	}
	if _, err := s.GetProject(ctx, orgA.ID, projB.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetProject cross-org: want ErrNotFound, got %v", err)
	}
	entries, _, err := s.ListTimeEntries(ctx, orgA.ID, TimeEntryFilter{}, Page{})
	if err != nil {
		t.Fatalf("ListTimeEntries A: %v", err)
	}
	for _, e := range entries {
		if e.OrganizationID == orgB.ID {
			t.Fatalf("ListTimeEntries(A) leaked org B entry %s", e.ID)
		}
	}

	// RLS probe on component 2 tables: connect as a non-owner role (RLS
	// policies are bypassed for table owners, so the probe must not be one)
	// and confirm a bare time_entries scan inside tenant A sees only A's
	// rows. Mirrors the component 1 probe in postgres_test.go.
	probeRole := fmt.Sprintf("rls_probe_time_%d", os.Getpid())
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
			`GRANT SELECT ON organizations, time_entries, hour_entries, pay_periods, pay_period_definitions, projects, schedules, shifts TO `+quoteIdent(probeRole)); err != nil {
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

	var bare, scoped int
	err = probe.inTenant(ctx, orgA.ID, func(tx pgx.Tx) error {
		// No explicit organization_id predicate: only RLS stands between
		// tenant A and org B's rows.
		return tx.QueryRow(ctx, `SELECT count(*) FROM time_entries`).Scan(&bare)
	})
	if err != nil {
		t.Fatalf("probe bare scan: %v", err)
	}
	err = probe.inTenant(ctx, orgA.ID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM time_entries WHERE organization_id = $1::uuid`, orgA.ID).Scan(&scoped)
	})
	if err != nil {
		t.Fatalf("probe scoped scan: %v", err)
	}
	if bare != scoped {
		t.Fatalf("RLS probe: bare scan saw %d rows, org A has %d (isolation broken)", bare, scoped)
	}
}
