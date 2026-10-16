// Package store — leave (component 3): time-off policies with an accrual
// engine (Annually/Monthly/PayPeriod/PayrollHours), carry-forward and
// expiry, leave requests with frozen snapshots, ordered multi-step approval
// chains, a per-period leave-balance ledger, and holiday calendars.
//
// Design notes:
//   - Money/payroll are untouched here: the only payroll touchpoint is
//     TimeOffRequest.IsUnpaid, which component 5 (payroll) consumes to deduct
//     unpaid leave from pay. Unpaid requests never touch balances.
//   - Dates are YYYY-MM-DD strings (no timezone ambiguity); the database
//     stores them as DATE.
//   - Accrue / ApplyCarryForward / ApplyExpiry / WorkingDays /
//     RecalculateBalance are pure functions (no I/O, no clock) so the
//     accrual engine is fully table-testable. RecalculateBalance is
//     idempotent: same inputs produce the same LeaveBalance.
//   - The balance ledger is annual (Period "2006"). Requests are attributed
//     to the period containing their start date.
//   - Carry-forward needs the previous period's stored balance, which a pure
//     function cannot load: RecalculateBalance always yields Carried=0 and
//     documents it; the store's RecalculateLeaveBalance applies
//     ApplyCarryForward from the stored previous-period balance on top.
//   - Requests are never hard-deleted: CancelTimeOffRequest flips status to
//     cancelled so the audit trail (TimeOffHistory via the hash-chained
//     audit log) stays referentially intact.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors for the leave domain. Use errors.Is to match.
var (
	// ErrInsufficientBalance means a paid leave request exceeds the
	// employee's available balance for the period.
	ErrInsufficientBalance = errors.New("store: insufficient leave balance")
	// ErrNotCurrentApprover means the decider is not the current pending
	// step of the approval chain (and is not an admin).
	ErrNotCurrentApprover = errors.New("store: not the current approver")
	// ErrInvalidTransition means the requested status change is not allowed
	// from the current status.
	ErrInvalidTransition = errors.New("store: invalid status transition")
)

// dateLayout is the YYYY-MM-DD format used for all leave dates.
const dateLayout = "2006-01-02"

// parseLeaveDate parses a YYYY-MM-DD date.
func parseLeaveDate(s string) (time.Time, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return time.Time{}, &ValidationError{Field: "date", Message: "must be YYYY-MM-DD"}
	}
	return t, nil
}

// periodEnd returns the last instant of an annual period label ("2006").
func periodEnd(period string) (time.Time, error) {
	y, err := time.Parse("2006", period)
	if err != nil {
		return time.Time{}, &ValidationError{Field: "period", Message: "must be YYYY"}
	}
	return time.Date(y.Year(), 12, 31, 23, 59, 59, 0, time.UTC), nil
}

// AccrualMode selects how leave accrues under a TimeOffPolicy.
type AccrualMode string

const (
	// AccrualAnnually grants AccrualAmount days per year, prorated by
	// months employed in the first year.
	AccrualAnnually AccrualMode = "annually"
	// AccrualMonthly grants AccrualAmount days per month employed.
	AccrualMonthly AccrualMode = "monthly"
	// AccrualPayPeriod grants AccrualAmount days per pay period. Until
	// component 2 (time tracking) defines pay periods, one pay period is
	// treated as one calendar month.
	AccrualPayPeriod AccrualMode = "pay_period"
	// AccrualPayrollHours accrues per hour worked. It yields 0 until
	// component 2 feeds worked hours; the mode is accepted so policies can
	// be created ahead of that integration.
	AccrualPayrollHours AccrualMode = "payroll_hours"
)

// ValidAccrualMode reports whether m is a known accrual mode.
func ValidAccrualMode(m AccrualMode) bool {
	switch m {
	case AccrualAnnually, AccrualMonthly, AccrualPayPeriod, AccrualPayrollHours:
		return true
	}
	return false
}

// TimeOffStatus is the lifecycle state of a leave request.
type TimeOffStatus string

const (
	TimeOffRequested TimeOffStatus = "requested"
	TimeOffApproved  TimeOffStatus = "approved"
	TimeOffRejected  TimeOffStatus = "rejected"
	TimeOffCancelled TimeOffStatus = "cancelled"
)

// ValidTimeOffStatus reports whether s is a known request status.
func ValidTimeOffStatus(s TimeOffStatus) bool {
	switch s {
	case TimeOffRequested, TimeOffApproved, TimeOffRejected, TimeOffCancelled:
		return true
	}
	return false
}

// ApprovalStatus is the state of one step in an approval chain.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
)

// ValidApprovalStatus reports whether s is a known approval status.
func ValidApprovalStatus(s ApprovalStatus) bool {
	switch s {
	case ApprovalPending, ApprovalApproved, ApprovalRejected:
		return true
	}
	return false
}

// TimeOffPolicy is a leave policy: its accrual engine plus carry-forward and
// expiry rules. Versioned for optimistic concurrency.
type TimeOffPolicy struct {
	ID              string      `json:"id"`
	OrganizationID  string      `json:"organization_id"`
	Name            string      `json:"name"`
	AccrualMode     AccrualMode `json:"accrual_mode"`
	AccrualAmount   float64     `json:"accrual_amount"`    // days granted per accrual period
	CarryForwardCap float64     `json:"carry_forward_cap"` // max days carried into the next period; <= 0 disables carry
	ExpiryMonths    int         `json:"expiry_months"`     // months after period end when carried leave lapses; <= 0 never expires
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	Version         int64       `json:"version"` // optimistic concurrency token
}

// Validate checks the policy's shape.
func (p TimeOffPolicy) Validate() error {
	if strings.TrimSpace(p.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(p.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	if !ValidAccrualMode(p.AccrualMode) {
		return &ValidationError{Field: "accrual_mode", Message: "unknown accrual mode"}
	}
	if p.AccrualAmount < 0 {
		return &ValidationError{Field: "accrual_amount", Message: "must not be negative"}
	}
	if p.CarryForwardCap < 0 {
		return &ValidationError{Field: "carry_forward_cap", Message: "must not be negative"}
	}
	if p.ExpiryMonths < 0 {
		return &ValidationError{Field: "expiry_months", Message: "must not be negative"}
	}
	return nil
}

// LeaveBalance is one employee's computed balance for one policy and one
// annual period. Balances are derived (see RecalculateBalance), never
// hand-edited: Balance = Accrued + Carried - Expired - Taken, floored at 0.
type LeaveBalance struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	EmployeeID     string    `json:"employee_id"`
	PolicyID       string    `json:"policy_id"`
	Period         string    `json:"period"` // annual label, e.g. "2026"
	Accrued        float64   `json:"accrued"`
	Taken          float64   `json:"taken"`
	Carried        float64   `json:"carried"`
	Expired        float64   `json:"expired"`
	Balance        float64   `json:"balance"`
	ComputedAt     time.Time `json:"computed_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Version        int64     `json:"version"` // optimistic concurrency token
}

// Validate checks the balance's shape.
func (b LeaveBalance) Validate() error {
	if strings.TrimSpace(b.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(b.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if strings.TrimSpace(b.PolicyID) == "" {
		return &ValidationError{Field: "policy_id", Message: "required"}
	}
	if _, err := periodEnd(b.Period); err != nil {
		return &ValidationError{Field: "period", Message: "must be YYYY"}
	}
	return nil
}

// TimeOffRequest is a leave request. Snapshot freezes the policy rules and
// the balance at request time (Jibble's snapshot-over-edits pattern); Days
// is store-computed from dates and holidays. ApproverIDs is write-only at
// creation: the store builds the approval chain from it atomically, and it
// is not persisted on the request row.
type TimeOffRequest struct {
	ID                string          `json:"id"`
	OrganizationID    string          `json:"organization_id"`
	EmployeeID        string          `json:"employee_id"`
	PolicyID          string          `json:"policy_id"`
	HolidayCalendarID *string         `json:"holiday_calendar_id,omitempty"`
	StartDate         string          `json:"start_date"` // YYYY-MM-DD
	EndDate           string          `json:"end_date"`   // YYYY-MM-DD, inclusive
	Days              float64         `json:"days"`       // store-computed working days
	Snapshot          json.RawMessage `json:"snapshot,omitempty"`
	Status            TimeOffStatus   `json:"status"`
	IsUnpaid          bool            `json:"is_unpaid"` // true: skips balance checks/deduction; component 5 deducts from pay
	ApproverIDs       []string        `json:"approver_ids,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
	Version           int64           `json:"version"` // optimistic concurrency token
}

// Validate checks the request's shape (the store computes Days and Status).
func (r TimeOffRequest) Validate() error {
	if strings.TrimSpace(r.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(r.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if strings.TrimSpace(r.PolicyID) == "" {
		return &ValidationError{Field: "policy_id", Message: "required"}
	}
	start, err := parseLeaveDate(r.StartDate)
	if err != nil {
		return &ValidationError{Field: "start_date", Message: "must be YYYY-MM-DD"}
	}
	end, err := parseLeaveDate(r.EndDate)
	if err != nil {
		return &ValidationError{Field: "end_date", Message: "must be YYYY-MM-DD"}
	}
	if end.Before(start) {
		return &ValidationError{Field: "end_date", Message: "must not be before start_date"}
	}
	if r.Status != "" && !ValidTimeOffStatus(r.Status) {
		return &ValidationError{Field: "status", Message: "unknown time-off status"}
	}
	return nil
}

// startTime parses StartDate; the caller must have validated first.
func (r TimeOffRequest) startTime() time.Time {
	t, _ := time.Parse(dateLayout, r.StartDate)
	return t
}

// endTime parses EndDate; the caller must have validated first.
func (r TimeOffRequest) endTime() time.Time {
	t, _ := time.Parse(dateLayout, r.EndDate)
	return t
}

// TimeOffApproval is one step of a request's ordered approval chain.
// Sequence is 1-based; the current step is the lowest-sequence pending one.
// DecidedBy records who actually decided (usually the designated approver;
// an admin override differs).
type TimeOffApproval struct {
	ID                 string         `json:"id"`
	OrganizationID     string         `json:"organization_id"`
	RequestID          string         `json:"request_id"`
	Sequence           int            `json:"sequence"`
	ApproverEmployeeID string         `json:"approver_employee_id"`
	Status             ApprovalStatus `json:"status"`
	DecidedBy          *string        `json:"decided_by,omitempty"`
	DecidedAt          *time.Time     `json:"decided_at,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
}

// Validate checks the approval step's shape.
func (a TimeOffApproval) Validate() error {
	if strings.TrimSpace(a.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(a.RequestID) == "" {
		return &ValidationError{Field: "request_id", Message: "required"}
	}
	if strings.TrimSpace(a.ApproverEmployeeID) == "" {
		return &ValidationError{Field: "approver_employee_id", Message: "required"}
	}
	if a.Sequence < 0 {
		return &ValidationError{Field: "sequence", Message: "must not be negative"}
	}
	if a.Status != "" && !ValidApprovalStatus(a.Status) {
		return &ValidationError{Field: "status", Message: "unknown approval status"}
	}
	return nil
}

// HolidayCalendar groups public holidays, e.g. one per country.
type HolidayCalendar struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	CountryCode    string    `json:"country_code"` // MY | ID
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Version        int64     `json:"version"` // optimistic concurrency token
}

// Validate checks the calendar's shape.
func (c HolidayCalendar) Validate() error {
	if strings.TrimSpace(c.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(c.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	if c.CountryCode != "MY" && c.CountryCode != "ID" {
		return &ValidationError{Field: "country_code", Message: "must be MY or ID"}
	}
	return nil
}

// Holiday is one public holiday on a calendar. (CalendarID, Date) is unique.
type Holiday struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	CalendarID     string    `json:"calendar_id"`
	Date           string    `json:"date"` // YYYY-MM-DD
	Name           string    `json:"name"`
	CreatedAt      time.Time `json:"created_at"`
}

// Validate checks the holiday's shape.
func (h Holiday) Validate() error {
	if strings.TrimSpace(h.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(h.CalendarID) == "" {
		return &ValidationError{Field: "calendar_id", Message: "required"}
	}
	if _, err := parseLeaveDate(h.Date); err != nil {
		return &ValidationError{Field: "date", Message: "must be YYYY-MM-DD"}
	}
	if strings.TrimSpace(h.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	return nil
}

// dateTime parses Date; the caller must have validated first.
func (h Holiday) dateTime() time.Time {
	t, _ := time.Parse(dateLayout, h.Date)
	return t
}

// ---------------------------------------------------------------------------
// Accrual engine (pure functions).
// ---------------------------------------------------------------------------

// fullMonthsBetween returns whole months between start and asOf, clamped at
// zero. A month counts once asOf reaches the same day-of-month as start.
func fullMonthsBetween(start, asOf time.Time) int {
	if asOf.Before(start) {
		return 0
	}
	sy, sm, sd := start.Date()
	ay, am, ad := asOf.Date()
	months := (ay-sy)*12 + int(am-sm)
	if ad < sd {
		months--
	}
	if months < 0 {
		return 0
	}
	return months
}

// MonthsEmployed returns whole months between the employee's work start and
// asOf, falling back to record creation when no start date is set.
func MonthsEmployed(e Employee, asOf time.Time) int {
	start := e.CreatedAt
	if e.WorkStartDate != nil && !e.WorkStartDate.IsZero() {
		start = *e.WorkStartDate
	}
	return fullMonthsBetween(start, asOf)
}

// Accrue returns the leave days accrued under the policy as of asOf, given
// monthsEmployed whole months of service.
//
//   - annually: AccrualAmount per year, prorated by months/12 in the first
//     year (a full year grants the whole amount).
//   - monthly: AccrualAmount per month employed.
//   - pay_period: treated as monthly until component 2 defines pay periods.
//   - payroll_hours: 0 — accrues per hour worked, which needs component 2's
//     time-tracking data; the mode exists so policies can be created now.
func Accrue(pol TimeOffPolicy, asOf time.Time, monthsEmployed int) float64 {
	if monthsEmployed < 0 {
		monthsEmployed = 0
	}
	switch pol.AccrualMode {
	case AccrualAnnually:
		m := monthsEmployed
		if m > 12 {
			m = 12
		}
		return pol.AccrualAmount * float64(m) / 12
	case AccrualMonthly, AccrualPayPeriod:
		return pol.AccrualAmount * float64(monthsEmployed)
	case AccrualPayrollHours:
		return 0
	default:
		return 0
	}
}

// ApplyCarryForward computes the days carried from a previous period's final
// balance into the next period, capped at the policy's CarryForwardCap. A
// non-positive cap disables carry-forward entirely.
func ApplyCarryForward(prev LeaveBalance, pol TimeOffPolicy) float64 {
	if pol.CarryForwardCap <= 0 {
		return 0
	}
	remaining := prev.Accrued + prev.Carried - prev.Expired - prev.Taken
	if remaining < 0 {
		remaining = 0
	}
	if remaining > pol.CarryForwardCap {
		return pol.CarryForwardCap
	}
	return remaining
}

// ApplyExpiry returns how many carried days lapse as of asOf: when
// ExpiryMonths is positive and asOf is past (period end + ExpiryMonths), the
// carried chunk lapses. It is capped at the available pool so the balance
// never goes negative from expiry alone.
func ApplyExpiry(bal LeaveBalance, pol TimeOffPolicy, asOf time.Time) float64 {
	if pol.ExpiryMonths <= 0 || bal.Carried <= 0 {
		return 0
	}
	end, err := periodEnd(bal.Period)
	if err != nil {
		return 0
	}
	if !asOf.After(end.AddDate(0, pol.ExpiryMonths, 0)) {
		return 0
	}
	available := bal.Accrued + bal.Carried - bal.Taken
	if available < 0 {
		available = 0
	}
	if bal.Carried > available {
		return available
	}
	return bal.Carried
}

// WorkingDays counts weekdays (Monday–Friday) in the inclusive [start, end]
// range, excluding the given holidays (matched by calendar date).
func WorkingDays(start, end time.Time, holidays []Holiday) float64 {
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	end = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	if end.Before(start) {
		return 0
	}
	off := make(map[string]bool, len(holidays))
	for _, h := range holidays {
		if t, err := time.Parse(dateLayout, h.Date); err == nil {
			off[t.Format(dateLayout)] = true
		}
	}
	var days float64
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		if off[d.Format(dateLayout)] {
			continue
		}
		days++
	}
	return days
}

// RecalculateBalance recomputes an employee's annual balance purely from its
// inputs — same inputs always produce the same LeaveBalance (ComputedAt is
// asOf, so the function is idempotent).
//
//   - Period is asOf's calendar year.
//   - Taken sums working days of paid requests (status requested or
//     approved) whose start date falls in the period. Rejected/cancelled
//     requests and unpaid leave never deduct. Request.Days is NOT trusted:
//     days are recomputed from dates + holidays so later holiday-calendar
//     edits are reflected (the stored Days remain the creation-time
//     snapshot for audit).
//   - Carried is always 0 here: carry-forward needs the previous period's
//     stored balance, which only the store can load. The store's
//     RecalculateLeaveBalance applies ApplyCarryForward on top.
//
// Multi-year requests are attributed wholly to the period containing their
// start date (documented simplification).
func RecalculateBalance(pol TimeOffPolicy, emp Employee, requests []TimeOffRequest, holidays []Holiday, asOf time.Time) LeaveBalance {
	asOf = asOf.UTC()
	period := asOf.Format("2006")
	accrued := Accrue(pol, asOf, MonthsEmployed(emp, asOf))
	var taken float64
	for _, r := range requests {
		if r.PolicyID != pol.ID || r.EmployeeID != emp.ID {
			continue
		}
		if r.IsUnpaid {
			continue
		}
		if r.Status != TimeOffRequested && r.Status != TimeOffApproved {
			continue
		}
		start, err := time.Parse(dateLayout, r.StartDate)
		if err != nil {
			continue
		}
		end, err := time.Parse(dateLayout, r.EndDate)
		if err != nil {
			continue
		}
		if start.Format("2006") != period {
			continue
		}
		taken += WorkingDays(start, end, holidays)
	}
	balance := accrued - taken
	if balance < 0 {
		balance = 0
	}
	return LeaveBalance{
		OrganizationID: emp.OrganizationID,
		EmployeeID:     emp.ID,
		PolicyID:       pol.ID,
		Period:         period,
		Accrued:        accrued,
		Taken:          taken,
		Carried:        0,
		Expired:        0,
		Balance:        balance,
		ComputedAt:     asOf,
	}
}

// finalizeBalance applies carry-forward (from the stored previous-period
// balance, nil when absent) and expiry onto a recalculated balance. It
// returns the balance and the unfloored pool value (for sufficiency checks).
func finalizeBalance(bal LeaveBalance, prev *LeaveBalance, pol TimeOffPolicy, asOf time.Time) (LeaveBalance, float64) {
	if prev != nil {
		bal.Carried = ApplyCarryForward(*prev, pol)
	}
	bal.Expired = ApplyExpiry(bal, pol, asOf)
	raw := bal.Accrued + bal.Carried - bal.Expired - bal.Taken
	if raw < 0 {
		bal.Balance = 0
	} else {
		bal.Balance = raw
	}
	return bal, raw
}

// resolveDecision validates one approval decision against the chain and
// computes its outcome. steps must be ordered by sequence. It returns the
// step to mark decided (nil for an admin decision on a chainless request)
// and the request's new status, which is unchanged when the chain still has
// pending steps after an approval.
func resolveDecision(req *TimeOffRequest, steps []TimeOffApproval, approverEmployeeID string, approved bool, asAdmin bool) (*TimeOffApproval, TimeOffStatus, error) {
	if req.Status != TimeOffRequested {
		return nil, "", fmt.Errorf("%w: request is %s", ErrInvalidTransition, req.Status)
	}
	var current *TimeOffApproval
	for i := range steps {
		if steps[i].Status == ApprovalPending {
			current = &steps[i]
			break
		}
	}
	if current == nil {
		// Chainless request: only an admin can decide it.
		if !asAdmin {
			return nil, "", fmt.Errorf("%w: request has no pending approval step", ErrNotCurrentApprover)
		}
		if approved {
			return nil, TimeOffApproved, nil
		}
		return nil, TimeOffRejected, nil
	}
	if current.ApproverEmployeeID != approverEmployeeID && !asAdmin {
		return nil, "", fmt.Errorf("%w: current approver is %s", ErrNotCurrentApprover, current.ApproverEmployeeID)
	}
	step := *current
	if approved {
		step.Status = ApprovalApproved
	} else {
		step.Status = ApprovalRejected
	}
	now := time.Now().UTC()
	step.DecidedAt = &now
	if approverEmployeeID != "" {
		step.DecidedBy = &approverEmployeeID
	}
	newStatus := req.Status
	if !approved {
		newStatus = TimeOffRejected
	} else {
		pending := false
		for i := range steps {
			s := steps[i]
			if s.ID == step.ID {
				s = step
			}
			if s.Status == ApprovalPending {
				pending = true
				break
			}
		}
		if !pending {
			newStatus = TimeOffApproved
		}
	}
	return &step, newStatus, nil
}

// ---------------------------------------------------------------------------
// Filters.
// ---------------------------------------------------------------------------

// TimeOffRequestFilter selects leave requests. Zero values mean "any".
type TimeOffRequestFilter struct {
	EmployeeID string
	PolicyID   string
	Status     TimeOffStatus
}

// LeaveBalanceFilter selects leave balances. Zero values mean "any".
type LeaveBalanceFilter struct {
	EmployeeID string
	PolicyID   string
	Period     string
}

// BulkApprovalResult is the per-request outcome of a bulk approval.
type BulkApprovalResult struct {
	RequestID string `json:"request_id"`
	Approved  bool   `json:"approved"`
	Reason    string `json:"reason,omitempty"`
}

// BulkApprovalError is returned by BulkApproveTimeOffRequests when at least
// one request cannot be approved. Nothing is applied: the operation is
// all-or-nothing, so callers can fix the reported requests and retry.
type BulkApprovalError struct {
	Results []BulkApprovalResult
}

func (e *BulkApprovalError) Error() string {
	n := 0
	for _, r := range e.Results {
		if !r.Approved {
			n++
		}
	}
	return "store: bulk approval failed for some requests"
}

// LeaveStore is the component 3 (leave) contract: time-off policies with an
// accrual engine, leave requests with snapshots, ordered approval chains,
// the balance ledger, and holiday calendars.
//
// Worker C embeds this into Store. Implementations must be safe for
// concurrent use and must deep-copy values across the boundary.
type LeaveStore interface {
	// Time-off policies.
	// CreateTimeOffPolicy stores a new policy. An empty ID is generated;
	// Version starts at 1.
	CreateTimeOffPolicy(ctx context.Context, p TimeOffPolicy) (TimeOffPolicy, error)
	// GetTimeOffPolicy returns the policy or ErrNotFound.
	GetTimeOffPolicy(ctx context.Context, orgID, id string) (TimeOffPolicy, error)
	// UpdateTimeOffPolicy applies p as the new state iff p.Version matches.
	// Returns ErrVersionConflict on mismatch, ErrNotFound when unknown.
	UpdateTimeOffPolicy(ctx context.Context, p TimeOffPolicy) (TimeOffPolicy, error)
	// DeleteTimeOffPolicy removes a policy that has no requests or balances
	// referencing it; otherwise ErrNotEmpty.
	DeleteTimeOffPolicy(ctx context.Context, orgID, id string) error
	// ListTimeOffPolicies returns the organization's policies ordered by
	// (created_at, id), paged.
	ListTimeOffPolicies(ctx context.Context, orgID string, p Page) (policies []TimeOffPolicy, next string, err error)

	// Leave balances (derived; recomputed, never hand-edited).
	// GetLeaveBalance returns the stored balance or ErrNotFound.
	GetLeaveBalance(ctx context.Context, orgID, employeeID, policyID, period string) (LeaveBalance, error)
	// ListLeaveBalances returns matching balances ordered by
	// (created_at, id), paged.
	ListLeaveBalances(ctx context.Context, orgID string, f LeaveBalanceFilter, p Page) (balances []LeaveBalance, next string, err error)
	// RecalculateLeaveBalance recomputes the employee's balance for asOf's
	// year from the policy, the employee's tenure, and paid
	// requested/approved requests (working days via holidays), applies
	// carry-forward from the stored previous-period balance and expiry,
	// then upserts the result. Idempotent: same inputs, same stored
	// balance.
	RecalculateLeaveBalance(ctx context.Context, orgID, employeeID, policyID string, asOf time.Time, holidays []Holiday) (LeaveBalance, error)

	// Leave requests.
	// CreateTimeOffRequest stores a new request with status requested.
	// Days is computed from dates + holidays (the store is authoritative).
	// r.ApproverIDs builds the approval chain atomically (empty = no
	// chain; only an admin can then decide). Paid requests check the
	// recalculated balance and return ErrInsufficientBalance when short;
	// unpaid requests skip the check. Snapshot freezes the policy rules
	// and the balance at request time.
	CreateTimeOffRequest(ctx context.Context, r TimeOffRequest, holidays []Holiday) (TimeOffRequest, error)
	// GetTimeOffRequest returns the request or ErrNotFound.
	GetTimeOffRequest(ctx context.Context, orgID, id string) (TimeOffRequest, error)
	// UpdateTimeOffRequest applies r as the new state iff r.Version
	// matches. Only requested requests can be edited (dates/days);
	// otherwise ErrInvalidTransition. Days is recomputed from dates +
	// holidays.
	UpdateTimeOffRequest(ctx context.Context, r TimeOffRequest, holidays []Holiday) (TimeOffRequest, error)
	// CancelTimeOffRequest flips a requested or approved request to
	// cancelled iff version matches; otherwise ErrInvalidTransition.
	CancelTimeOffRequest(ctx context.Context, orgID, id string, version int64) (TimeOffRequest, error)
	// ListTimeOffRequests returns matching requests ordered by
	// (created_at, id), paged.
	ListTimeOffRequests(ctx context.Context, orgID string, f TimeOffRequestFilter, p Page) (requests []TimeOffRequest, next string, err error)
	// ListApprovedLeaveOnDate returns approved requests overlapping date
	// (YYYY-MM-DD), ordered by (created_at, id), paged. Powers
	// who-is-on-leave.
	ListApprovedLeaveOnDate(ctx context.Context, orgID, date string, p Page) (requests []TimeOffRequest, next string, err error)

	// Approval chains.
	// CreateTimeOffApproval appends one step to a requested request's
	// chain. Sequence <= 0 auto-assigns max+1. The approver must be a
	// joined employee in the organization.
	CreateTimeOffApproval(ctx context.Context, a TimeOffApproval) (TimeOffApproval, error)
	// ListTimeOffApprovals returns the request's steps ordered by
	// sequence, paged.
	ListTimeOffApprovals(ctx context.Context, orgID, requestID string, p Page) (approvals []TimeOffApproval, next string, err error)
	// DecideTimeOffApproval records the current pending step's decision.
	// Only the step's designated approver may decide, unless asAdmin. An
	// approval advances the chain; when no pending steps remain the
	// request becomes approved. Any rejection makes the request rejected.
	// A chainless requested request can only be decided by an admin.
	// requestVersion guards the request row (ErrVersionConflict on
	// mismatch). Returns the updated request and the full chain.
	DecideTimeOffApproval(ctx context.Context, orgID, requestID, approverEmployeeID string, approved bool, requestVersion int64, asAdmin bool) (TimeOffRequest, []TimeOffApproval, error)
	// BulkApproveTimeOffRequests approves every listed request or none:
	// each must be requested and decidable by approverEmployeeID (or
	// asAdmin). Returns *BulkApprovalError with per-request reasons when
	// any request fails, applying nothing.
	BulkApproveTimeOffRequests(ctx context.Context, orgID string, ids []string, approverEmployeeID string, asAdmin bool) ([]TimeOffRequest, error)

	// Holiday calendars.
	// CreateHolidayCalendar stores a new calendar. An empty ID is
	// generated; Version starts at 1.
	CreateHolidayCalendar(ctx context.Context, c HolidayCalendar) (HolidayCalendar, error)
	// GetHolidayCalendar returns the calendar or ErrNotFound.
	GetHolidayCalendar(ctx context.Context, orgID, id string) (HolidayCalendar, error)
	// UpdateHolidayCalendar applies c as the new state iff c.Version
	// matches. Returns ErrVersionConflict on mismatch, ErrNotFound when
	// unknown.
	UpdateHolidayCalendar(ctx context.Context, c HolidayCalendar) (HolidayCalendar, error)
	// DeleteHolidayCalendar removes a calendar with no holidays;
	// otherwise ErrNotEmpty.
	DeleteHolidayCalendar(ctx context.Context, orgID, id string) error
	// ListHolidayCalendars returns the organization's calendars ordered by
	// (created_at, id), paged.
	ListHolidayCalendars(ctx context.Context, orgID string, p Page) (calendars []HolidayCalendar, next string, err error)

	// Holidays.
	// CreateHoliday stores a holiday; (calendar_id, date) must be unique
	// (ErrAlreadyExists on clash).
	CreateHoliday(ctx context.Context, h Holiday) (Holiday, error)
	// GetHoliday returns the holiday or ErrNotFound.
	GetHoliday(ctx context.Context, orgID, id string) (Holiday, error)
	// DeleteHoliday removes a holiday. ErrNotFound when unknown.
	DeleteHoliday(ctx context.Context, orgID, id string) error
	// ListHolidays returns the calendar's holidays ordered by date, paged.
	ListHolidays(ctx context.Context, orgID, calendarID string, p Page) (holidays []Holiday, next string, err error)
}
