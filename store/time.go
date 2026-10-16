// Package store — component 2: time tracking.
//
// This file holds the time-tracking domain types (Validate pattern copied
// from component 1), the TimeTrackingStore interface, the new sentinel
// errors, and the pure overtime engine.
//
// Design notes for high load (massive concurrent AI-agent workload):
//   - Clock-in/out/breaks are a per-employee state machine enforced inside
//     the store: exactly one open entry may exist per employee. The memory
//     adapter serializes on its mutex; the Postgres adapter takes a
//     transaction-scoped advisory lock per employee
//     (pg_advisory_xact_lock) before reading the latest entry.
//   - Corrections never edit: CorrectTimeEntry freezes the original entry
//     (IsLocked, NextID set) and creates a superseding head entry linked
//     via PrevID. Listings show head entries only (NextID == nil) unless
//     asked for history; superseded rows stay for audit.
//   - Pay periods gate mutability: entries whose occurred_at falls inside a
//     Submitted or Approved period are frozen (ErrPeriodLocked). Approving
//     a period sets IsLocked on every entry it covers; reopening unlocks
//     them again.
//   - ComputeOvertime is pure: given clock entries + rules it returns the
//     hour breakdown. The API's /overtime/preview endpoint is a thin
//     wrapper over it.
package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Time-tracking sentinel errors. Use errors.Is to match.
var (
	// ErrAlreadyClockedIn means the employee already has an open time
	// entry (clocked in, or on a break) and cannot clock in again.
	ErrAlreadyClockedIn = errors.New("store: already clocked in")
	// ErrPeriodLocked means the entry sits inside a Submitted or Approved
	// pay period (or is otherwise frozen) and cannot be changed.
	ErrPeriodLocked = errors.New("store: pay period locked")
	// ErrInvalidTransition is declared in store/leave.go (component 3) and
	// shared here: it means a clock or pay-period state transition is not
	// allowed from the current state.
)

// TimeEntryKind is the punch kind of a time entry.
type TimeEntryKind string

const (
	TimeEntryIn         TimeEntryKind = "in"
	TimeEntryOut        TimeEntryKind = "out"
	TimeEntryStartBreak TimeEntryKind = "start_break"
	TimeEntryEndBreak   TimeEntryKind = "end_break"
)

// ValidTimeEntryKind reports whether k is a known entry kind.
func ValidTimeEntryKind(k TimeEntryKind) bool {
	switch k {
	case TimeEntryIn, TimeEntryOut, TimeEntryStartBreak, TimeEntryEndBreak:
		return true
	}
	return false
}

// Time-entry client types. Agents are first-class: ai_assistant is a valid
// client type (mirrors Jibble's TimeEntryClientType.AiAssistant).
const (
	TimeEntryClientWeb         = "web"
	TimeEntryClientMobile      = "mobile"
	TimeEntryClientDesktop     = "desktop"
	TimeEntryClientKiosk       = "kiosk"
	TimeEntryClientAIAssistant = "ai_assistant"
	TimeEntryClientSlack       = "slack"
	TimeEntryClientTeams       = "teams"
)

// ValidTimeEntryClient reports whether c is a known client type.
func ValidTimeEntryClient(c string) bool {
	switch c {
	case TimeEntryClientWeb, TimeEntryClientMobile, TimeEntryClientDesktop,
		TimeEntryClientKiosk, TimeEntryClientAIAssistant,
		TimeEntryClientSlack, TimeEntryClientTeams:
		return true
	}
	return false
}

// TimeEntry is one punch: clock in/out or break start/end. Project/activity/
// location are optional cost-center tags; GPS is optional (the MVP skips
// geofencing — coordinates are recorded, not enforced).
//
// PrevID/NextID form the correction chain: a correction creates a new head
// entry with PrevID set and freezes the original (IsLocked, NextID set).
// IsLocked is also set by pay-period approval. DeletedAt is the soft-delete
// marker.
type TimeEntry struct {
	ID             string        `json:"id"`
	OrganizationID string        `json:"organization_id"`
	EmployeeID     string        `json:"employee_id"`
	Kind           TimeEntryKind `json:"kind"`
	OccurredAt     time.Time     `json:"occurred_at"`
	ProjectID      *string       `json:"project_id,omitempty"`
	ActivityID     *string       `json:"activity_id,omitempty"` // forward ref; activities land in a later component
	LocationID     *string       `json:"location_id,omitempty"` // forward ref; locations land in a later component
	Latitude       *float64      `json:"latitude,omitempty"`
	Longitude      *float64      `json:"longitude,omitempty"`
	Address        *string       `json:"address,omitempty"`
	ClientType     string        `json:"client_type"`
	PrevID         *string       `json:"prev_id,omitempty"`
	NextID         *string       `json:"next_id,omitempty"`
	IsLocked       bool          `json:"is_locked"`
	Version        int64         `json:"version"` // optimistic concurrency token
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
	DeletedAt      *time.Time    `json:"deleted_at,omitempty"`
}

// Validate checks the time entry's shape.
func (e TimeEntry) Validate() error {
	if strings.TrimSpace(e.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(e.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if !ValidTimeEntryKind(e.Kind) {
		return &ValidationError{Field: "kind", Message: "must be one of in, out, start_break, end_break"}
	}
	if e.OccurredAt.IsZero() {
		return &ValidationError{Field: "occurred_at", Message: "required"}
	}
	if e.ClientType != "" && !ValidTimeEntryClient(e.ClientType) {
		return &ValidationError{Field: "client_type", Message: "unknown client type"}
	}
	if e.Latitude != nil && (*e.Latitude < -90 || *e.Latitude > 90) {
		return &ValidationError{Field: "latitude", Message: "must be between -90 and 90"}
	}
	if e.Longitude != nil && (*e.Longitude < -180 || *e.Longitude > 180) {
		return &ValidationError{Field: "longitude", Message: "must be between -180 and 180"}
	}
	return nil
}

// openKind reports whether k is a kind that leaves an entry "open" — the
// employee is still on the clock (possibly on a break).
func openKind(k TimeEntryKind) bool {
	return k == TimeEntryIn || k == TimeEntryStartBreak || k == TimeEntryEndBreak
}

// HourEntry is a manual hour log (the Jibble HourEntry equivalent): a date,
// a number of hours, an optional project tag, and a note.
type HourEntry struct {
	ID             string     `json:"id"`
	OrganizationID string     `json:"organization_id"`
	EmployeeID     string     `json:"employee_id"`
	WorkDate       time.Time  `json:"work_date"` // date, midnight UTC
	Hours          float64    `json:"hours"`
	ProjectID      *string    `json:"project_id,omitempty"`
	Note           string     `json:"note,omitempty"`
	Version        int64      `json:"version"` // optimistic concurrency token
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
}

// Validate checks the hour entry's shape.
func (e HourEntry) Validate() error {
	if strings.TrimSpace(e.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(e.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if e.WorkDate.IsZero() {
		return &ValidationError{Field: "work_date", Message: "required"}
	}
	if e.Hours <= 0 {
		return &ValidationError{Field: "hours", Message: "must be positive"}
	}
	return nil
}

// Project is a cost-center tag for time entries and hour entries.
type Project struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	ClientName     string    `json:"client_name,omitempty"`
	Version        int64     `json:"version"` // optimistic concurrency token
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Validate checks the project's shape.
func (p Project) Validate() error {
	if strings.TrimSpace(p.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(p.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	return nil
}

// PayPeriodCadence is how a pay-period definition slices the calendar.
type PayPeriodCadence string

const (
	PayPeriodCadenceMonthly      PayPeriodCadence = "Monthly"
	PayPeriodCadenceTwiceInMonth PayPeriodCadence = "TwiceInMonth"
	PayPeriodCadenceWeekly       PayPeriodCadence = "Weekly"
)

// ValidPayPeriodCadence reports whether c is a known cadence.
func ValidPayPeriodCadence(c PayPeriodCadence) bool {
	switch c {
	case PayPeriodCadenceMonthly, PayPeriodCadenceTwiceInMonth, PayPeriodCadenceWeekly:
		return true
	}
	return false
}

// PayPeriodDefinition describes how pay periods are generated: a name, a
// cadence, and an anchor day (day-of-month for Monthly, weekday 0=Sunday for
// Weekly; ignored for TwiceInMonth which always splits 1st–15th / 16th–end).
type PayPeriodDefinition struct {
	ID             string           `json:"id"`
	OrganizationID string           `json:"organization_id"`
	Name           string           `json:"name"`
	Cadence        PayPeriodCadence `json:"cadence"`
	AnchorDay      int              `json:"anchor_day"`
	Version        int64            `json:"version"` // optimistic concurrency token
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
}

// Validate checks the pay-period definition's shape.
func (d PayPeriodDefinition) Validate() error {
	if strings.TrimSpace(d.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(d.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	if !ValidPayPeriodCadence(d.Cadence) {
		return &ValidationError{Field: "cadence", Message: "must be one of Monthly, TwiceInMonth, Weekly"}
	}
	switch d.Cadence {
	case PayPeriodCadenceMonthly:
		if d.AnchorDay < 1 || d.AnchorDay > 31 {
			return &ValidationError{Field: "anchor_day", Message: "must be 1-31 for Monthly"}
		}
	case PayPeriodCadenceWeekly:
		if d.AnchorDay < 0 || d.AnchorDay > 6 {
			return &ValidationError{Field: "anchor_day", Message: "must be 0-6 (Sunday=0) for Weekly"}
		}
	}
	return nil
}

// ComputePayPeriodRange returns the [start, end) UTC range of the period
// containing t under the definition's cadence. All bounds are day-aligned
// (midnight UTC).
func ComputePayPeriodRange(d PayPeriodDefinition, t time.Time) (start, end time.Time, err error) {
	if err := d.Validate(); err != nil {
		return time.Time{}, time.Time{}, err
	}
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	switch d.Cadence {
	case PayPeriodCadenceMonthly:
		// Periods run anchorDay -> anchorDay of the next month, clamped to
		// the month's length (anchor 31 in February becomes Feb 28/29).
		anchor := min(d.AnchorDay, daysIn(day.Year(), day.Month()))
		start = time.Date(day.Year(), day.Month(), anchor, 0, 0, 0, 0, time.UTC)
		if day.Before(start) {
			prev := start.AddDate(0, -1, 0)
			anchor = min(d.AnchorDay, daysIn(prev.Year(), prev.Month()))
			start = time.Date(prev.Year(), prev.Month(), anchor, 0, 0, 0, 0, time.UTC)
		}
		// Compute the following month arithmetically (AddDate would
		// overflow: Jan 31 + 1 month = Mar 3) and clamp to its length.
		nm := time.Date(start.Year(), start.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		endAnchor := min(d.AnchorDay, daysIn(nm.Year(), nm.Month()))
		end = time.Date(nm.Year(), nm.Month(), endAnchor, 0, 0, 0, 0, time.UTC)
	case PayPeriodCadenceTwiceInMonth:
		if day.Day() <= 15 {
			start = time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
			end = time.Date(day.Year(), day.Month(), 16, 0, 0, 0, 0, time.UTC)
		} else {
			start = time.Date(day.Year(), day.Month(), 16, 0, 0, 0, 0, time.UTC)
			end = start.AddDate(0, 1, 0)
			end = time.Date(end.Year(), end.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
	case PayPeriodCadenceWeekly:
		// Periods run anchorWeekday -> +7 days.
		back := (int(day.Weekday()) - d.AnchorDay + 7) % 7
		start = day.AddDate(0, 0, -back)
		end = start.AddDate(0, 0, 7)
	default:
		return time.Time{}, time.Time{}, &ValidationError{Field: "cadence", Message: "must be one of Monthly, TwiceInMonth, Weekly"}
	}
	return start, end, nil
}

func daysIn(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// PayPeriodStatus is the pay-period lifecycle state.
type PayPeriodStatus string

const (
	PayPeriodOpen      PayPeriodStatus = "Open"
	PayPeriodSubmitted PayPeriodStatus = "Submitted"
	PayPeriodApproved  PayPeriodStatus = "Approved"
	PayPeriodRejected  PayPeriodStatus = "Rejected"
	PayPeriodReopened  PayPeriodStatus = "Reopened"
)

// ValidPayPeriodStatus reports whether s is a known status.
func ValidPayPeriodStatus(s PayPeriodStatus) bool {
	switch s {
	case PayPeriodOpen, PayPeriodSubmitted, PayPeriodApproved, PayPeriodRejected, PayPeriodReopened:
		return true
	}
	return false
}

// validPayPeriodTransition reports whether a status change is allowed.
// Open -> Submitted -> Approved/Rejected; Approved -> Reopened (audit);
// Rejected/Reopened -> Submitted (resubmit).
func validPayPeriodTransition(from, to PayPeriodStatus) bool {
	switch from {
	case PayPeriodOpen:
		return to == PayPeriodSubmitted
	case PayPeriodSubmitted:
		return to == PayPeriodApproved || to == PayPeriodRejected
	case PayPeriodRejected, PayPeriodReopened:
		return to == PayPeriodSubmitted
	case PayPeriodApproved:
		return to == PayPeriodReopened
	}
	return false
}

// periodFrozen reports whether entries inside a period of this status are
// immutable. Submitted freezes the timesheet while it awaits a decision;
// Approved freezes it permanently (until Reopened).
func periodFrozen(s PayPeriodStatus) bool {
	return s == PayPeriodSubmitted || s == PayPeriodApproved
}

// PayPeriod is one concrete period instance: the definition it was
// generated from, its [start, end) range, and its lifecycle status.
type PayPeriod struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organization_id"`
	DefinitionID   string          `json:"definition_id"`
	PeriodStart    time.Time       `json:"period_start"`
	PeriodEnd      time.Time       `json:"period_end"`
	Status         PayPeriodStatus `json:"status"`
	Version        int64           `json:"version"` // optimistic concurrency token
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// Validate checks the pay period's shape.
func (p PayPeriod) Validate() error {
	if strings.TrimSpace(p.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(p.DefinitionID) == "" {
		return &ValidationError{Field: "definition_id", Message: "required"}
	}
	if p.PeriodStart.IsZero() || p.PeriodEnd.IsZero() {
		return &ValidationError{Field: "period_start", Message: "period_start and period_end are required"}
	}
	if !p.PeriodEnd.After(p.PeriodStart) {
		return &ValidationError{Field: "period_end", Message: "must be after period_start"}
	}
	if !ValidPayPeriodStatus(p.Status) {
		return &ValidationError{Field: "status", Message: "unknown pay period status"}
	}
	return nil
}

// ScheduleKind is the schedule shape.
type ScheduleKind string

const (
	ScheduleFixed    ScheduleKind = "Fixed"
	ScheduleFlexible ScheduleKind = "Flexible"
	ScheduleWeekly   ScheduleKind = "Weekly"
)

// ValidScheduleKind reports whether k is a known schedule kind.
func ValidScheduleKind(k ScheduleKind) bool {
	switch k {
	case ScheduleFixed, ScheduleFlexible, ScheduleWeekly:
		return true
	}
	return false
}

// Schedule is an employee's expected-hours template (Jibble schedule
// basics; shift instances are separate Shift rows).
type Schedule struct {
	ID                 string       `json:"id"`
	OrganizationID     string       `json:"organization_id"`
	EmployeeID         string       `json:"employee_id"`
	Kind               ScheduleKind `json:"kind"`
	ExpectedDailyHours float64      `json:"expected_daily_hours"`
	Version            int64        `json:"version"` // optimistic concurrency token
	CreatedAt          time.Time    `json:"created_at"`
	UpdatedAt          time.Time    `json:"updated_at"`
}

// Validate checks the schedule's shape.
func (s Schedule) Validate() error {
	if strings.TrimSpace(s.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(s.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if !ValidScheduleKind(s.Kind) {
		return &ValidationError{Field: "kind", Message: "must be one of Fixed, Flexible, Weekly"}
	}
	if s.ExpectedDailyHours <= 0 {
		return &ValidationError{Field: "expected_daily_hours", Message: "must be positive"}
	}
	return nil
}

// Shift is one scheduled work instance (first-class shift scheduling — the
// documented Jibble gap). Published shifts are visible to the employee;
// unpublished ones are drafts the manager is still arranging.
type Shift struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	EmployeeID     string    `json:"employee_id"`
	ShiftDate      time.Time `json:"shift_date"` // date, midnight UTC
	StartsAt       time.Time `json:"starts_at"`
	EndsAt         time.Time `json:"ends_at"`
	Published      bool      `json:"published"`
	Version        int64     `json:"version"` // optimistic concurrency token
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Validate checks the shift's shape.
func (s Shift) Validate() error {
	if strings.TrimSpace(s.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(s.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if s.ShiftDate.IsZero() {
		return &ValidationError{Field: "shift_date", Message: "required"}
	}
	if s.StartsAt.IsZero() || s.EndsAt.IsZero() {
		return &ValidationError{Field: "starts_at", Message: "starts_at and ends_at are required"}
	}
	if !s.EndsAt.After(s.StartsAt) {
		return &ValidationError{Field: "ends_at", Message: "must be after starts_at"}
	}
	return nil
}

// --- filters ---

// TimeEntryFilter selects time entries. Zero values mean "any".
// IncludeHistory also returns superseded entries (NextID != nil);
// IncludeDeleted also returns soft-deleted entries.
type TimeEntryFilter struct {
	EmployeeID     string
	Kind           TimeEntryKind
	From           *time.Time
	To             *time.Time
	IncludeDeleted bool
	IncludeHistory bool
}

// HourEntryFilter selects hour entries. Zero values mean "any".
type HourEntryFilter struct {
	EmployeeID     string
	From           *time.Time
	To             *time.Time
	IncludeDeleted bool
}

// ShiftFilter selects shifts. Zero values mean "any".
type ShiftFilter struct {
	EmployeeID string
	From       *time.Time
	To         *time.Time
	Published  *bool
}

// PayPeriodFilter selects pay periods. Zero values mean "any".
type PayPeriodFilter struct {
	DefinitionID string
	Status       PayPeriodStatus
}

// --- overtime engine ---

// OvertimeRules carries the thresholds and multipliers for ComputeOvertime.
// Non-positive thresholds disable that tier (all hours fall through to
// regular). Multipliers are informational on the result buckets: the result
// reports hours per bucket; pay = hours x multiplier x rate.
type OvertimeRules struct {
	DailyThresholdHours  float64 `json:"daily_threshold_hours"`  // regular hours/day before 1.5x
	DoubleThresholdHours float64 `json:"double_threshold_hours"` // hours/day before 2x
	WeeklyThresholdHours float64 `json:"weekly_threshold_hours"` // regular hours/week before weekly 1.5x
	OvertimeMult         float64 `json:"overtime_mult"`
	DoubleMult           float64 `json:"double_mult"`
	RestDayMult          float64 `json:"rest_day_mult"`
	HolidayMult          float64 `json:"holiday_mult"`
}

// OvertimeResult is the hour breakdown from ComputeOvertime.
type OvertimeResult struct {
	RegularHours float64 `json:"regular_hours"`
	Overtime15   float64 `json:"overtime_15"`    // hours at OvertimeMult (daily + weekly overflow)
	Overtime2x   float64 `json:"overtime_2x"`    // hours at DoubleMult
	RestDayHours float64 `json:"rest_day_hours"` // weekend hours at RestDayMult
	HolidayHours float64 `json:"holiday_hours"`  // public-holiday hours at HolidayMult
}

// TotalHours returns the sum of every bucket.
func (r OvertimeResult) TotalHours() float64 {
	return r.RegularHours + r.Overtime15 + r.Overtime2x + r.RestDayHours + r.HolidayHours
}

// ComputeOvertime pairs clock entries into work intervals (In->Out, breaks
// subtracted) and buckets the hours per the rules. It is pure: no I/O.
//
// Pairing rules:
//   - Entries sort by (occurred_at, created_at, id). In opens work,
//     StartBreak opens a break, EndBreak closes the break, Out closes work.
//     Stray closers (Out with no open In) and never-closed openers are
//     ignored — an employee still clocked in has no computable hours yet.
//   - Soft-deleted (DeletedAt != nil) and superseded (NextID != nil)
//     entries are skipped: previews reflect the current record.
//   - Hours group by calendar day in UTC. Days in holidays (keys
//     "YYYY-MM-DD") bucket fully to HolidayHours; Saturdays/Sundays bucket
//     fully to RestDayHours; normal days split regular / 1.5x / 2x at the
//     daily and double thresholds.
//   - Weekly overflow: regular hours above WeeklyThresholdHours within an
//     ISO week move from RegularHours to Overtime15.
func ComputeOvertime(entries []TimeEntry, rules OvertimeRules, holidays map[string]bool) OvertimeResult {
	live := make([]TimeEntry, 0, len(entries))
	for _, e := range entries {
		if e.DeletedAt != nil || e.NextID != nil {
			continue
		}
		live = append(live, e)
	}
	sortByCursor(live, func(e TimeEntry) cursorKey { return cursorKey{e.OccurredAt, e.ID} })

	type span struct{ start, end time.Time }
	var work, brk []span
	var openWork, openBreak []time.Time
	for _, e := range live {
		switch e.Kind {
		case TimeEntryIn:
			openWork = append(openWork, e.OccurredAt)
		case TimeEntryStartBreak:
			openBreak = append(openBreak, e.OccurredAt)
		case TimeEntryEndBreak:
			if n := len(openBreak); n > 0 {
				s := openBreak[n-1]
				openBreak = openBreak[:n-1]
				if e.OccurredAt.After(s) {
					brk = append(brk, span{s, e.OccurredAt})
				}
			}
		case TimeEntryOut:
			if n := len(openWork); n > 0 {
				s := openWork[n-1]
				openWork = openWork[:n-1]
				if e.OccurredAt.After(s) {
					work = append(work, span{s, e.OccurredAt})
				}
			}
		}
	}

	// Net seconds per UTC day. An interval is attributed to the day its
	// start falls on (overnight shifts are rare in the MY/ID scope and the
	// REA keeps day attribution simple).
	daySecs := map[string]float64{}
	sub := func(spans []span, sign float64) {
		for _, s := range spans {
			key := s.start.UTC().Format("2006-01-02")
			daySecs[key] += sign * s.end.Sub(s.start).Seconds()
		}
	}
	sub(work, 1)
	sub(brk, -1)

	var res OvertimeResult
	weeklyRegular := map[string]float64{} // ISO week key -> regular hours
	isoWeek := func(day string) string {
		t, _ := time.Parse("2006-01-02", day)
		y, w := t.UTC().ISOWeek()
		return strings.TrimSpace(strings.Join([]string{itoa(y), itoa(w)}, "-W"))
	}
	for day, secs := range daySecs {
		hours := secs / 3600
		if hours <= 0 {
			continue
		}
		switch {
		case holidays[day]:
			res.HolidayHours += hours
		case isWeekend(day):
			res.RestDayHours += hours
		default:
			reg, ot15, ot2x := splitDay(hours, rules)
			res.RegularHours += reg
			res.Overtime15 += ot15
			res.Overtime2x += ot2x
			weeklyRegular[isoWeek(day)] += reg
		}
	}
	if rules.WeeklyThresholdHours > 0 {
		for _, reg := range weeklyRegular {
			if overflow := reg - rules.WeeklyThresholdHours; overflow > 0 {
				res.RegularHours -= overflow
				res.Overtime15 += overflow
			}
		}
	}
	return res
}

// splitDay applies the daily tiers to one normal day's hours.
func splitDay(hours float64, rules OvertimeRules) (reg, ot15, ot2x float64) {
	daily, dbl := rules.DailyThresholdHours, rules.DoubleThresholdHours
	if daily <= 0 {
		return hours, 0, 0
	}
	reg = min(hours, daily)
	above := hours - reg
	if dbl > daily {
		ot15 = min(above, dbl-daily)
		ot2x = above - ot15
	} else {
		ot15 = above
	}
	return reg, ot15, ot2x
}

func isWeekend(day string) bool {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return false
	}
	wd := t.Weekday()
	return wd == time.Saturday || wd == time.Sunday
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TimeTrackingStore is the component 2 (time tracking) contract: punches
// with a per-employee clock state machine, manual hour entries, projects,
// pay-period definitions and the pay-period lifecycle, schedules, and
// first-class shifts.
//
// All methods are safe for concurrent use and deep-copy values across the
// boundary, matching the component 1 contract.
type TimeTrackingStore interface {
	// Projects.
	// CreateProject stores a project. Version starts at 1.
	CreateProject(ctx context.Context, p Project) (Project, error)
	// GetProject returns the project or ErrNotFound.
	GetProject(ctx context.Context, orgID, id string) (Project, error)
	// UpdateProject applies p iff p.Version matches. ErrVersionConflict on
	// mismatch, ErrNotFound when unknown.
	UpdateProject(ctx context.Context, p Project) (Project, error)
	// DeleteProject hard-deletes a project; referencing time/hour entries
	// keep their rows with project_id set to NULL.
	DeleteProject(ctx context.Context, orgID, id string) error
	// ListProjects returns the org's projects ordered by (created_at, id),
	// paged.
	ListProjects(ctx context.Context, orgID string, pg Page) ([]Project, string, error)

	// Clock state machine. Each call is atomic per employee: exactly one
	// open entry may exist. The entry's kind is forced by the method; any
	// kind carried on e is ignored.
	// ClockIn creates an In entry; ErrAlreadyClockedIn when an open entry
	// exists. ClockOut closes the open entry; ErrNotFound when none is
	// open. StartBreak requires a working open entry (In/EndBreak);
	// EndBreak requires an open StartBreak; otherwise ErrInvalidTransition.
	// All four return ErrPeriodLocked when the entry would fall inside a
	// Submitted or Approved pay period.
	ClockIn(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error)
	ClockOut(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error)
	StartBreak(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error)
	EndBreak(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error)
	// OpenTimeEntry returns the employee's latest open entry (kind In,
	// StartBreak or EndBreak), or open=false when fully clocked out.
	OpenTimeEntry(ctx context.Context, orgID, employeeID string) (entry TimeEntry, open bool, err error)

	// Time entries.
	// CreateTimeEntry stores a backfill/manual punch without enforcing the
	// clock state machine (the clock methods above do that). Still gated
	// on the pay-period lock.
	CreateTimeEntry(ctx context.Context, e TimeEntry) (TimeEntry, error)
	// GetTimeEntry returns the entry or ErrNotFound.
	GetTimeEntry(ctx context.Context, orgID, id string) (TimeEntry, error)
	// UpdateTimeEntry applies e iff e.Version matches and the entry is
	// mutable (!IsLocked and not inside a frozen period); otherwise
	// ErrPeriodLocked. Kind, employee, and the correction chain cannot be
	// changed here — use CorrectTimeEntry.
	UpdateTimeEntry(ctx context.Context, e TimeEntry) (TimeEntry, error)
	// DeleteTimeEntry soft-deletes iff version matches; ErrPeriodLocked
	// when frozen.
	DeleteTimeEntry(ctx context.Context, orgID, id string, version int64) (TimeEntry, error)
	// CorrectTimeEntry freezes the original (IsLocked, NextID set) and
	// creates a superseding head entry linked via PrevID. The original must
	// be a head entry (NextID == nil) and mutable; otherwise
	// ErrInvalidTransition / ErrPeriodLocked.
	CorrectTimeEntry(ctx context.Context, orgID, id string, correction TimeEntry) (TimeEntry, error)
	// ListTimeEntries returns matching entries ordered by (created_at, id),
	// paged. Superseded and soft-deleted rows are excluded unless the
	// filter asks for them.
	ListTimeEntries(ctx context.Context, orgID string, f TimeEntryFilter, pg Page) ([]TimeEntry, string, error)

	// Hour entries (manual logs).
	CreateHourEntry(ctx context.Context, e HourEntry) (HourEntry, error)
	GetHourEntry(ctx context.Context, orgID, id string) (HourEntry, error)
	// UpdateHourEntry applies e iff e.Version matches; ErrPeriodLocked when
	// the work date falls inside a frozen period.
	UpdateHourEntry(ctx context.Context, e HourEntry) (HourEntry, error)
	// DeleteHourEntry soft-deletes iff version matches.
	DeleteHourEntry(ctx context.Context, orgID, id string, version int64) (HourEntry, error)
	ListHourEntries(ctx context.Context, orgID string, f HourEntryFilter, pg Page) ([]HourEntry, string, error)

	// Pay-period definitions.
	CreatePayPeriodDefinition(ctx context.Context, d PayPeriodDefinition) (PayPeriodDefinition, error)
	GetPayPeriodDefinition(ctx context.Context, orgID, id string) (PayPeriodDefinition, error)
	UpdatePayPeriodDefinition(ctx context.Context, d PayPeriodDefinition) (PayPeriodDefinition, error)
	// DeletePayPeriodDefinition removes a definition with no periods;
	// ErrNotEmpty when periods exist.
	DeletePayPeriodDefinition(ctx context.Context, orgID, id string) error
	ListPayPeriodDefinitions(ctx context.Context, orgID string, pg Page) ([]PayPeriodDefinition, string, error)

	// Pay periods. CreatePayPeriod generates the [start, end) range from
	// the definition's cadence around periodStart; the unique
	// (definition_id, period_start) keeps generation idempotent-ish
	// (duplicate generation returns ErrAlreadyExists).
	CreatePayPeriod(ctx context.Context, orgID, definitionID string, periodStart time.Time) (PayPeriod, error)
	GetPayPeriod(ctx context.Context, orgID, id string) (PayPeriod, error)
	ListPayPeriods(ctx context.Context, orgID string, f PayPeriodFilter, pg Page) ([]PayPeriod, string, error)
	// Transitions are version-checked and follow the lifecycle
	// Open->Submitted->Approved/Rejected, Approved->Reopened,
	// Rejected/Reopened->Submitted; anything else is ErrInvalidTransition.
	// ApprovePayPeriod locks every entry the period covers (IsLocked);
	// ReopenPayPeriod unlocks them.
	SubmitPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error)
	ApprovePayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error)
	RejectPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error)
	ReopenPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error)

	// Schedules.
	CreateSchedule(ctx context.Context, s Schedule) (Schedule, error)
	GetSchedule(ctx context.Context, orgID, id string) (Schedule, error)
	UpdateSchedule(ctx context.Context, s Schedule) (Schedule, error)
	DeleteSchedule(ctx context.Context, orgID, id string) error
	// ListSchedules returns the org's schedules (employeeID filters to one
	// employee when non-empty), ordered by (created_at, id), paged.
	ListSchedules(ctx context.Context, orgID, employeeID string, pg Page) ([]Schedule, string, error)

	// Shifts.
	CreateShift(ctx context.Context, s Shift) (Shift, error)
	GetShift(ctx context.Context, orgID, id string) (Shift, error)
	UpdateShift(ctx context.Context, s Shift) (Shift, error)
	DeleteShift(ctx context.Context, orgID, id string) error
	ListShifts(ctx context.Context, orgID string, f ShiftFilter, pg Page) ([]Shift, string, error)
	// PublishShifts marks shifts published (visible to employees);
	// idempotent; returns the number of shifts newly published.
	PublishShifts(ctx context.Context, orgID string, ids []string) (int64, error)
}
