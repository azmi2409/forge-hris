package store

// Component 2 store tests: the pure overtime engine (table-driven) and the
// MemoryStore time-tracking behavior (clock state machine, versioning,
// corrections, pay-period lifecycle, publish idempotency).

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func punch(kind TimeEntryKind, at string) TimeEntry {
	return TimeEntry{
		OrganizationID: "org-ot",
		EmployeeID:     "emp-ot",
		Kind:           kind,
		OccurredAt:     mustTime(at),
		ClientType:     TimeEntryClientWeb,
	}
}

func TestComputeOvertime(t *testing.T) {
	rules := OvertimeRules{
		DailyThresholdHours:  8,
		DoubleThresholdHours: 12,
		WeeklyThresholdHours: 40,
		OvertimeMult:         1.5,
		DoubleMult:           2,
		RestDayMult:          2,
		HolidayMult:          3,
	}
	cases := []struct {
		name     string
		entries  []TimeEntry
		holidays map[string]bool
		want     OvertimeResult
	}{
		{
			name: "daily overtime",
			entries: []TimeEntry{
				punch(TimeEntryIn, "2026-10-12T09:00:00Z"),
				punch(TimeEntryOut, "2026-10-12T18:00:00Z"), // 9h Monday
			},
			want: OvertimeResult{RegularHours: 8, Overtime15: 1},
		},
		{
			name: "double time beyond double threshold",
			entries: []TimeEntry{
				punch(TimeEntryIn, "2026-10-12T09:00:00Z"),
				punch(TimeEntryOut, "2026-10-12T22:00:00Z"), // 13h Monday
			},
			want: OvertimeResult{RegularHours: 8, Overtime15: 4, Overtime2x: 1},
		},
		{
			name: "unpaid break subtracted",
			entries: []TimeEntry{
				punch(TimeEntryIn, "2026-10-12T09:00:00Z"),
				punch(TimeEntryStartBreak, "2026-10-12T12:00:00Z"),
				punch(TimeEntryEndBreak, "2026-10-12T12:30:00Z"),
				punch(TimeEntryOut, "2026-10-12T17:30:00Z"), // 8h net Monday
			},
			want: OvertimeResult{RegularHours: 8},
		},
		{
			name: "weekly overflow moves regular to 1.5x",
			entries: func() []TimeEntry {
				// Mon-Fri 2026-10-12..16, 9h each.
				var out []TimeEntry
				for d := 12; d <= 16; d++ {
					out = append(out,
						punch(TimeEntryIn, fmt.Sprintf("2026-10-%02dT09:00:00Z", d)),
						punch(TimeEntryOut, fmt.Sprintf("2026-10-%02dT18:00:00Z", d)),
					)
				}
				return out
			}(),
			// The runner below overrides the threshold to 35 and the
			// expectation to {RegularHours: 35, Overtime15: 10}.
			want: OvertimeResult{},
		},
		{
			name: "rest day at rest multiplier",
			entries: []TimeEntry{
				punch(TimeEntryIn, "2026-10-17T09:00:00Z"),
				punch(TimeEntryOut, "2026-10-17T17:00:00Z"), // Saturday
			},
			want: OvertimeResult{RestDayHours: 8},
		},
		{
			name: "holiday at holiday multiplier",
			entries: []TimeEntry{
				punch(TimeEntryIn, "2026-10-12T09:00:00Z"),
				punch(TimeEntryOut, "2026-10-12T18:00:00Z"), // Monday, but a holiday
			},
			holidays: map[string]bool{"2026-10-12": true},
			want:     OvertimeResult{HolidayHours: 9},
		},
		{
			name: "unpaired clock-in ignored",
			entries: []TimeEntry{
				punch(TimeEntryIn, "2026-10-12T09:00:00Z"),
			},
			want: OvertimeResult{},
		},
		{
			name: "stray clock-out ignored",
			entries: []TimeEntry{
				punch(TimeEntryOut, "2026-10-12T18:00:00Z"),
			},
			want: OvertimeResult{},
		},
		{
			name: "superseded and deleted entries skipped",
			entries: func() []TimeEntry {
				old := punch(TimeEntryIn, "2026-10-12T09:00:00Z")
				old.NextID = strPtr("newer")
				del := punch(TimeEntryOut, "2026-10-12T18:00:00Z")
				now := time.Now().UTC()
				del.DeletedAt = &now
				return []TimeEntry{old, del}
			}(),
			want: OvertimeResult{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := rules
			want := tc.want
			if tc.name == "weekly overflow moves regular to 1.5x" {
				r.WeeklyThresholdHours = 35
				// 5x9h: per-day 8 reg + 1 ot15 -> reg 40 overflows by 5.
				want = OvertimeResult{RegularHours: 35, Overtime15: 10}
			}
			got := ComputeOvertime(tc.entries, r, tc.holidays)
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestComputePayPeriodRange(t *testing.T) {
	cases := []struct {
		name      string
		def       PayPeriodDefinition
		at        string
		wantStart string
		wantEnd   string
	}{
		{
			name:      "monthly anchor 1",
			def:       PayPeriodDefinition{OrganizationID: "org-test", Name: "m", Cadence: PayPeriodCadenceMonthly, AnchorDay: 1},
			at:        "2026-10-15T10:00:00Z",
			wantStart: "2026-10-01", wantEnd: "2026-11-01",
		},
		{
			name:      "monthly anchor 31 clamps in february",
			def:       PayPeriodDefinition{OrganizationID: "org-test", Name: "m", Cadence: PayPeriodCadenceMonthly, AnchorDay: 31},
			at:        "2026-02-15T10:00:00Z",
			wantStart: "2026-01-31", wantEnd: "2026-02-28",
		},
		{
			name:      "twice in month first half",
			def:       PayPeriodDefinition{OrganizationID: "org-test", Name: "t", Cadence: PayPeriodCadenceTwiceInMonth},
			at:        "2026-10-05T10:00:00Z",
			wantStart: "2026-10-01", wantEnd: "2026-10-16",
		},
		{
			name:      "twice in month second half",
			def:       PayPeriodDefinition{OrganizationID: "org-test", Name: "t", Cadence: PayPeriodCadenceTwiceInMonth},
			at:        "2026-10-20T10:00:00Z",
			wantStart: "2026-10-16", wantEnd: "2026-11-01",
		},
		{
			name:      "weekly anchor monday",
			def:       PayPeriodDefinition{OrganizationID: "org-test", Name: "w", Cadence: PayPeriodCadenceWeekly, AnchorDay: 1},
			at:        "2026-10-14T10:00:00Z", // Wednesday
			wantStart: "2026-10-12", wantEnd: "2026-10-19",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, err := ComputePayPeriodRange(tc.def, mustTime(tc.at))
			if err != nil {
				t.Fatalf("ComputePayPeriodRange: %v", err)
			}
			if got := start.Format("2006-01-02"); got != tc.wantStart {
				t.Fatalf("start = %s, want %s", got, tc.wantStart)
			}
			if got := end.Format("2006-01-02"); got != tc.wantEnd {
				t.Fatalf("end = %s, want %s", got, tc.wantEnd)
			}
		})
	}
}

// newTimeTestOrg creates an org + joined employee for time tests.
func newTimeTestOrg(t *testing.T, st *MemoryStore, slug string) (string, string) {
	t.Helper()
	ctx := context.Background()
	org, err := st.CreateOrganization(ctx, Organization{
		Name: "Time " + slug, Slug: "time-" + slug, CountryCode: "MY",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	emp, err := st.CreateEmployee(ctx, Employee{
		OrganizationID: org.ID,
		EmployeeNumber: "T-" + slug,
		FirstName:      "Punch",
		Status:         InvitationJoined,
	})
	if err != nil {
		t.Fatalf("CreateEmployee: %v", err)
	}
	return org.ID, emp.ID
}

func TestMemoryClockFlow(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	orgID, empID := newTimeTestOrg(t, st, "flow")

	in, err := st.ClockIn(ctx, orgID, empID, TimeEntry{ClientType: TimeEntryClientAIAssistant})
	if err != nil {
		t.Fatalf("ClockIn: %v", err)
	}
	if in.Kind != TimeEntryIn || in.ClientType != TimeEntryClientAIAssistant {
		t.Fatalf("bad clock-in: %+v", in)
	}
	if _, err := st.ClockIn(ctx, orgID, empID, TimeEntry{}); !errors.Is(err, ErrAlreadyClockedIn) {
		t.Fatalf("double clock-in: want ErrAlreadyClockedIn, got %v", err)
	}
	if open, isOpen, err := st.OpenTimeEntry(ctx, orgID, empID); err != nil || !isOpen || open.ID != in.ID {
		t.Fatalf("OpenTimeEntry: %v %v %+v", open, isOpen, err)
	}

	if _, err := st.StartBreak(ctx, orgID, empID, TimeEntry{}); err != nil {
		t.Fatalf("StartBreak: %v", err)
	}
	if _, err := st.StartBreak(ctx, orgID, empID, TimeEntry{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double break: want ErrInvalidTransition, got %v", err)
	}
	if _, err := st.EndBreak(ctx, orgID, empID, TimeEntry{}); err != nil {
		t.Fatalf("EndBreak: %v", err)
	}
	if _, err := st.EndBreak(ctx, orgID, empID, TimeEntry{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("end break without break: want ErrInvalidTransition, got %v", err)
	}
	if _, err := st.ClockOut(ctx, orgID, empID, TimeEntry{}); err != nil {
		t.Fatalf("ClockOut: %v", err)
	}
	if _, isOpen, err := st.OpenTimeEntry(ctx, orgID, empID); err != nil || isOpen {
		t.Fatalf("after clock-out: open=%v err=%v", isOpen, err)
	}
	if _, err := st.ClockOut(ctx, orgID, empID, TimeEntry{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("clock-out with no open entry: want ErrNotFound, got %v", err)
	}
}

func TestMemoryClockInRace(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	orgID, empID := newTimeTestOrg(t, st, "race")

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.ClockIn(ctx, orgID, empID, TimeEntry{})
		}(i)
	}
	wg.Wait()
	won, lost := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrAlreadyClockedIn):
			lost++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || lost != n-1 {
		t.Fatalf("want exactly 1 winner and %d losers, got %d/%d", n-1, won, lost)
	}
}

func TestMemoryTimeEntryCRUD(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	orgID, empID := newTimeTestOrg(t, st, "crud")

	created, err := st.CreateTimeEntry(ctx, TimeEntry{
		OrganizationID: orgID,
		EmployeeID:     empID,
		Kind:           TimeEntryIn,
		OccurredAt:     mustTime("2026-10-12T09:00:00Z"),
	})
	if err != nil {
		t.Fatalf("CreateTimeEntry: %v", err)
	}
	if created.Version != 1 {
		t.Fatalf("version = %d, want 1", created.Version)
	}

	got, err := st.GetTimeEntry(ctx, orgID, created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("GetTimeEntry: %v", err)
	}
	// Mutating the returned copy must not affect the store.
	got.Kind = TimeEntryOut
	again, _ := st.GetTimeEntry(ctx, orgID, created.ID)
	if again.Kind != TimeEntryIn {
		t.Fatalf("store mutated through returned value")
	}

	// Version conflict.
	stale := created
	stale.Version = 999
	stale.OccurredAt = mustTime("2026-10-12T09:05:00Z")
	if _, err := st.UpdateTimeEntry(ctx, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale update: want ErrVersionConflict, got %v", err)
	}
	// Kind change is rejected.
	cur := created
	cur.Version = created.Version
	cur.Kind = TimeEntryOut
	if _, err := st.UpdateTimeEntry(ctx, cur); err == nil {
		t.Fatalf("kind change should be rejected")
	}
	// Legit sparse update.
	cur.Kind = TimeEntryIn
	addr := "Menara Test, KL"
	cur.Address = &addr
	updated, err := st.UpdateTimeEntry(ctx, cur)
	if err != nil {
		t.Fatalf("UpdateTimeEntry: %v", err)
	}
	if updated.Version != 2 || updated.Address == nil || *updated.Address != addr {
		t.Fatalf("bad update: %+v", updated)
	}

	// Soft delete hides from default lists.
	if _, err := st.DeleteTimeEntry(ctx, orgID, created.ID, updated.Version); err != nil {
		t.Fatalf("DeleteTimeEntry: %v", err)
	}
	list, _, err := st.ListTimeEntries(ctx, orgID, TimeEntryFilter{}, Page{})
	if err != nil {
		t.Fatalf("ListTimeEntries: %v", err)
	}
	for _, e := range list {
		if e.ID == created.ID {
			t.Fatalf("deleted entry listed")
		}
	}
	list, _, err = st.ListTimeEntries(ctx, orgID, TimeEntryFilter{IncludeDeleted: true}, Page{})
	if err != nil {
		t.Fatalf("ListTimeEntries include deleted: %v", err)
	}
	found := false
	for _, e := range list {
		if e.ID == created.ID && e.DeletedAt != nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("deleted entry not listed with IncludeDeleted")
	}
}

func TestMemoryCorrectTimeEntry(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	orgID, empID := newTimeTestOrg(t, st, "correct")

	orig, err := st.ClockIn(ctx, orgID, empID, TimeEntry{OccurredAt: mustTime("2026-10-12T09:00:00Z")})
	if err != nil {
		t.Fatalf("ClockIn: %v", err)
	}
	corrected, err := st.CorrectTimeEntry(ctx, orgID, orig.ID, TimeEntry{
		Kind:       TimeEntryIn,
		OccurredAt: mustTime("2026-10-12T08:55:00Z"),
	})
	if err != nil {
		t.Fatalf("CorrectTimeEntry: %v", err)
	}
	if corrected.PrevID == nil || *corrected.PrevID != orig.ID {
		t.Fatalf("correction not linked back: %+v", corrected)
	}
	if !corrected.OccurredAt.Equal(mustTime("2026-10-12T08:55:00Z")) {
		t.Fatalf("correction kept wrong time: %v", corrected.OccurredAt)
	}
	frozen, err := st.GetTimeEntry(ctx, orgID, orig.ID)
	if err != nil {
		t.Fatalf("GetTimeEntry orig: %v", err)
	}
	if frozen.NextID == nil || *frozen.NextID != corrected.ID || !frozen.IsLocked {
		t.Fatalf("original not frozen/linked: %+v", frozen)
	}
	// Default listing shows the head only; history opt-in shows both.
	list, _, err := st.ListTimeEntries(ctx, orgID, TimeEntryFilter{}, Page{})
	if err != nil {
		t.Fatalf("ListTimeEntries: %v", err)
	}
	if len(list) != 1 || list[0].ID != corrected.ID {
		t.Fatalf("default list should show only the head, got %d entries", len(list))
	}
	list, _, err = st.ListTimeEntries(ctx, orgID, TimeEntryFilter{IncludeHistory: true}, Page{})
	if err != nil {
		t.Fatalf("ListTimeEntries history: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("history list should show both, got %d", len(list))
	}
	// Correcting an already-corrected entry is rejected.
	if _, err := st.CorrectTimeEntry(ctx, orgID, orig.ID, TimeEntry{Kind: TimeEntryIn}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("double correction: want ErrInvalidTransition, got %v", err)
	}
}

func TestMemoryPayPeriodLifecycle(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	orgID, empID := newTimeTestOrg(t, st, "period")

	def, err := st.CreatePayPeriodDefinition(ctx, PayPeriodDefinition{
		OrganizationID: orgID, Name: "Monthly", Cadence: PayPeriodCadenceMonthly, AnchorDay: 1,
	})
	if err != nil {
		t.Fatalf("CreatePayPeriodDefinition: %v", err)
	}
	period, err := st.CreatePayPeriod(ctx, orgID, def.ID, mustTime("2026-10-15T00:00:00Z"))
	if err != nil {
		t.Fatalf("CreatePayPeriod: %v", err)
	}
	if got := period.PeriodStart.Format("2006-01-02"); got != "2026-10-01" {
		t.Fatalf("period start = %s", got)
	}
	if period.Status != PayPeriodOpen {
		t.Fatalf("status = %s", period.Status)
	}
	// Duplicate generation for the same range collides.
	if _, err := st.CreatePayPeriod(ctx, orgID, def.ID, mustTime("2026-10-20T00:00:00Z")); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate period: want ErrAlreadyExists, got %v", err)
	}
	// Approve straight from Open is an invalid transition.
	if _, err := st.ApprovePayPeriod(ctx, orgID, period.ID, period.Version); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("approve from Open: want ErrInvalidTransition, got %v", err)
	}

	entry, err := st.ClockIn(ctx, orgID, empID, TimeEntry{OccurredAt: mustTime("2026-10-12T09:00:00Z")})
	if err != nil {
		t.Fatalf("ClockIn: %v", err)
	}

	submitted, err := st.SubmitPayPeriod(ctx, orgID, period.ID, period.Version)
	if err != nil {
		t.Fatalf("SubmitPayPeriod: %v", err)
	}
	if submitted.Status != PayPeriodSubmitted {
		t.Fatalf("status = %s", submitted.Status)
	}
	// Entries inside a submitted period are frozen.
	if _, err := st.ClockOut(ctx, orgID, empID, TimeEntry{OccurredAt: mustTime("2026-10-12T18:00:00Z")}); !errors.Is(err, ErrPeriodLocked) {
		t.Fatalf("clock-out into submitted period: want ErrPeriodLocked, got %v", err)
	}

	approved, err := st.ApprovePayPeriod(ctx, orgID, period.ID, submitted.Version)
	if err != nil {
		t.Fatalf("ApprovePayPeriod: %v", err)
	}
	if approved.Status != PayPeriodApproved {
		t.Fatalf("status = %s", approved.Status)
	}
	locked, err := st.GetTimeEntry(ctx, orgID, entry.ID)
	if err != nil {
		t.Fatalf("GetTimeEntry: %v", err)
	}
	if !locked.IsLocked {
		t.Fatalf("approval did not lock covered entries")
	}
	upd := locked
	upd.Version = locked.Version
	if _, err := st.UpdateTimeEntry(ctx, upd); !errors.Is(err, ErrPeriodLocked) {
		t.Fatalf("update locked entry: want ErrPeriodLocked, got %v", err)
	}

	// Reopen unlocks; resubmit works again.
	reopened, err := st.ReopenPayPeriod(ctx, orgID, period.ID, approved.Version)
	if err != nil {
		t.Fatalf("ReopenPayPeriod: %v", err)
	}
	if reopened.Status != PayPeriodReopened {
		t.Fatalf("status = %s", reopened.Status)
	}
	unlocked, err := st.GetTimeEntry(ctx, orgID, entry.ID)
	if err != nil {
		t.Fatalf("GetTimeEntry: %v", err)
	}
	if unlocked.IsLocked {
		t.Fatalf("reopen did not unlock covered entries")
	}
	if _, err := st.SubmitPayPeriod(ctx, orgID, period.ID, reopened.Version); err != nil {
		t.Fatalf("resubmit after reopen: %v", err)
	}
	// Rejected -> Submitted is also a legal resubmit.
	if _, err := st.RejectPayPeriod(ctx, orgID, period.ID, reopened.Version+1); err != nil {
		t.Fatalf("RejectPayPeriod: %v", err)
	}
}

func TestMemoryHourEntryAndProject(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	orgID, empID := newTimeTestOrg(t, st, "hours")

	proj, err := st.CreateProject(ctx, Project{OrganizationID: orgID, Name: "Website", ClientName: "Acme"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	he, err := st.CreateHourEntry(ctx, HourEntry{
		OrganizationID: orgID,
		EmployeeID:     empID,
		WorkDate:       mustTime("2026-10-12T00:00:00Z"),
		Hours:          7.5,
		ProjectID:      &proj.ID,
		Note:           "deep work",
	})
	if err != nil {
		t.Fatalf("CreateHourEntry: %v", err)
	}
	if he.Hours != 7.5 || he.Note != "deep work" {
		t.Fatalf("bad hour entry: %+v", he)
	}
	upd := he
	upd.Version = he.Version
	upd.Hours = 8
	upd2, err := st.UpdateHourEntry(ctx, upd)
	if err != nil {
		t.Fatalf("UpdateHourEntry: %v", err)
	}
	if upd2.Version != 2 || upd2.Hours != 8 {
		t.Fatalf("bad update: %+v", upd2)
	}
	// Unknown project is rejected.
	badProj := "no-such-project"
	if _, err := st.CreateHourEntry(ctx, HourEntry{
		OrganizationID: orgID, EmployeeID: empID,
		WorkDate: mustTime("2026-10-12T00:00:00Z"), Hours: 1, ProjectID: &badProj,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown project: want ErrNotFound, got %v", err)
	}
	// Deleting the project nulls the FK on referencing rows.
	if err := st.DeleteProject(ctx, orgID, proj.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	got, err := st.GetHourEntry(ctx, orgID, he.ID)
	if err != nil {
		t.Fatalf("GetHourEntry: %v", err)
	}
	if got.ProjectID != nil {
		t.Fatalf("project FK not nulled after delete")
	}
}

func TestMemoryShiftPublish(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	orgID, empID := newTimeTestOrg(t, st, "shifts")

	mk := func(date string) Shift {
		sh, err := st.CreateShift(ctx, Shift{
			OrganizationID: orgID,
			EmployeeID:     empID,
			ShiftDate:      mustTime(date + "T00:00:00Z"),
			StartsAt:       mustTime(date + "T09:00:00Z"),
			EndsAt:         mustTime(date + "T17:00:00Z"),
		})
		if err != nil {
			t.Fatalf("CreateShift: %v", err)
		}
		return sh
	}
	a, b := mk("2026-10-19"), mk("2026-10-20")
	n, err := st.PublishShifts(ctx, orgID, []string{a.ID, b.ID})
	if err != nil || n != 2 {
		t.Fatalf("PublishShifts: %v %d", err, n)
	}
	// Idempotent: republishing counts zero.
	n, err = st.PublishShifts(ctx, orgID, []string{a.ID})
	if err != nil || n != 0 {
		t.Fatalf("republish: %v %d", err, n)
	}
	published := true
	list, _, err := st.ListShifts(ctx, orgID, ShiftFilter{Published: &published}, Page{})
	if err != nil {
		t.Fatalf("ListShifts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 published shifts, got %d", len(list))
	}
	if _, err := st.PublishShifts(ctx, orgID, []string{"no-such-shift"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publish unknown: want ErrNotFound, got %v", err)
	}
}

func TestMemoryScheduleCRUD(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	orgID, empID := newTimeTestOrg(t, st, "sched")

	sc, err := st.CreateSchedule(ctx, Schedule{
		OrganizationID: orgID, EmployeeID: empID,
		Kind: ScheduleFixed, ExpectedDailyHours: 8,
	})
	if err != nil {
		t.Fatalf("CreateSchedule: %v", err)
	}
	upd := sc
	upd.Version = sc.Version
	upd.ExpectedDailyHours = 7.5
	upd2, err := st.UpdateSchedule(ctx, upd)
	if err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	if upd2.ExpectedDailyHours != 7.5 || upd2.Version != 2 {
		t.Fatalf("bad update: %+v", upd2)
	}
	list, _, err := st.ListSchedules(ctx, orgID, empID, Page{})
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSchedules: %v %d", err, len(list))
	}
	if err := st.DeleteSchedule(ctx, orgID, sc.ID); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if _, err := st.GetSchedule(ctx, orgID, sc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted: want ErrNotFound, got %v", err)
	}
}
