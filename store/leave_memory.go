package store

// MemoryStore leave implementation (component 3). All methods hold s.mu and
// deep-copy across the boundary, mirroring the Postgres adapter's semantics.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Compile-time contract check (Worker C embeds LeaveStore into Store).
var _ LeaveStore = (*MemoryStore)(nil)

// bkey builds the composite map key for leave balances.
func bkey(orgID, employeeID, policyID, period string) string {
	return orgID + "\x00" + employeeID + "\x00" + policyID + "\x00" + period
}

func copyTimeOffPolicy(p TimeOffPolicy) TimeOffPolicy { return p }

func copyLeaveBalance(b LeaveBalance) LeaveBalance { return b }

func copyTimeOffRequest(r TimeOffRequest) TimeOffRequest {
	r.HolidayCalendarID = copyStrPtr(r.HolidayCalendarID)
	r.Snapshot = copyBytes(r.Snapshot)
	r.ApproverIDs = copyStrings(r.ApproverIDs)
	return r
}

func copyTimeOffApproval(a TimeOffApproval) TimeOffApproval {
	a.DecidedBy = copyStrPtr(a.DecidedBy)
	a.DecidedAt = copyTimePtr(a.DecidedAt)
	return a
}

func copyHolidayCalendar(c HolidayCalendar) HolidayCalendar { return c }

func copyHoliday(h Holiday) Holiday { return h }

// ---------------------------------------------------------------------------
// Time-off policies
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateTimeOffPolicy(ctx context.Context, p TimeOffPolicy) (TimeOffPolicy, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffPolicy{}, err
	}
	if err := p.Validate(); err != nil {
		return TimeOffPolicy{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	p.Version = 1

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[p.OrganizationID]; !ok {
		return TimeOffPolicy{}, fmt.Errorf("%w: organization %s", ErrNotFound, p.OrganizationID)
	}
	if _, ok := s.leavePolicies[gkey(p.OrganizationID, p.ID)]; ok {
		return TimeOffPolicy{}, fmt.Errorf("%w: time-off policy %s", ErrAlreadyExists, p.ID)
	}
	s.leavePolicies[gkey(p.OrganizationID, p.ID)] = p
	return copyTimeOffPolicy(p), nil
}

func (s *MemoryStore) GetTimeOffPolicy(ctx context.Context, orgID, id string) (TimeOffPolicy, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffPolicy{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.leavePolicies[gkey(orgID, id)]
	if !ok {
		return TimeOffPolicy{}, fmt.Errorf("%w: time-off policy %s", ErrNotFound, id)
	}
	return copyTimeOffPolicy(p), nil
}

func (s *MemoryStore) UpdateTimeOffPolicy(ctx context.Context, p TimeOffPolicy) (TimeOffPolicy, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffPolicy{}, err
	}
	if err := p.Validate(); err != nil {
		return TimeOffPolicy{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.leavePolicies[gkey(p.OrganizationID, p.ID)]
	if !ok {
		return TimeOffPolicy{}, fmt.Errorf("%w: time-off policy %s", ErrNotFound, p.ID)
	}
	if p.Version != cur.Version {
		return TimeOffPolicy{}, fmt.Errorf("%w: time-off policy %s", ErrVersionConflict, p.ID)
	}
	cur.Name = p.Name
	cur.AccrualMode = p.AccrualMode
	cur.AccrualAmount = p.AccrualAmount
	cur.CarryForwardCap = p.CarryForwardCap
	cur.ExpiryMonths = p.ExpiryMonths
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.leavePolicies[gkey(p.OrganizationID, p.ID)] = cur
	return copyTimeOffPolicy(cur), nil
}

func (s *MemoryStore) DeleteTimeOffPolicy(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.leavePolicies[gkey(orgID, id)]; !ok {
		return fmt.Errorf("%w: time-off policy %s", ErrNotFound, id)
	}
	for _, r := range s.leaveRequests {
		if r.OrganizationID == orgID && r.PolicyID == id {
			return fmt.Errorf("%w: policy %s still has leave requests", ErrNotEmpty, id)
		}
	}
	for _, b := range s.leaveBalances {
		if b.OrganizationID == orgID && b.PolicyID == id {
			return fmt.Errorf("%w: policy %s still has leave balances", ErrNotEmpty, id)
		}
	}
	delete(s.leavePolicies, gkey(orgID, id))
	return nil
}

func (s *MemoryStore) ListTimeOffPolicies(ctx context.Context, orgID string, p Page) ([]TimeOffPolicy, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TimeOffPolicy, 0)
	for _, pol := range s.leavePolicies {
		if pol.OrganizationID == orgID {
			out = append(out, copyTimeOffPolicy(pol))
		}
	}
	sortByCursor(out, func(pol TimeOffPolicy) cursorKey { return cursorKey{pol.CreatedAt, pol.ID} })
	return paginate(out, func(pol TimeOffPolicy) cursorKey { return cursorKey{pol.CreatedAt, pol.ID} }, p)
}

// ---------------------------------------------------------------------------
// Leave balances
// ---------------------------------------------------------------------------

// computeBalanceLocked recomputes the full balance (pure recalc +
// carry-forward from the stored previous-period balance + expiry) without
// persisting. It also returns the unfloored pool value for sufficiency
// checks. Callers must hold s.mu.
func (s *MemoryStore) computeBalanceLocked(orgID string, emp Employee, pol TimeOffPolicy, requests []TimeOffRequest, holidays []Holiday, asOf time.Time) (LeaveBalance, float64) {
	period := asOf.UTC().Format("2006")
	var prev *LeaveBalance
	if p, ok := s.leaveBalances[bkey(orgID, emp.ID, pol.ID, prevPeriod(period))]; ok {
		c := copyLeaveBalance(p)
		prev = &c
	}
	bal := RecalculateBalance(pol, emp, requests, holidays, asOf)
	return finalizeBalance(bal, prev, pol, asOf)
}

// prevPeriod returns the annual period label before the given one.
func prevPeriod(period string) string {
	if t, err := time.Parse("2006", period); err == nil {
		return t.AddDate(-1, 0, 0).Format("2006")
	}
	return ""
}

// leaveRequestsForLocked returns copies of the org's requests for the given
// employee+policy, excluding exceptID. Callers must hold s.mu.
func (s *MemoryStore) leaveRequestsForLocked(orgID, employeeID, policyID, exceptID string) []TimeOffRequest {
	var out []TimeOffRequest
	for _, r := range s.leaveRequests {
		if r.OrganizationID != orgID || r.EmployeeID != employeeID || r.PolicyID != policyID {
			continue
		}
		if exceptID != "" && r.ID == exceptID {
			continue
		}
		out = append(out, copyTimeOffRequest(r))
	}
	return out
}

func (s *MemoryStore) GetLeaveBalance(ctx context.Context, orgID, employeeID, policyID, period string) (LeaveBalance, error) {
	if err := ctx.Err(); err != nil {
		return LeaveBalance{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.leaveBalances[bkey(orgID, employeeID, policyID, period)]
	if !ok {
		return LeaveBalance{}, fmt.Errorf("%w: leave balance", ErrNotFound)
	}
	return copyLeaveBalance(b), nil
}

func (s *MemoryStore) ListLeaveBalances(ctx context.Context, orgID string, f LeaveBalanceFilter, p Page) ([]LeaveBalance, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]LeaveBalance, 0)
	for _, b := range s.leaveBalances {
		if b.OrganizationID != orgID {
			continue
		}
		if f.EmployeeID != "" && b.EmployeeID != f.EmployeeID {
			continue
		}
		if f.PolicyID != "" && b.PolicyID != f.PolicyID {
			continue
		}
		if f.Period != "" && b.Period != f.Period {
			continue
		}
		out = append(out, copyLeaveBalance(b))
	}
	sortByCursor(out, func(b LeaveBalance) cursorKey { return cursorKey{b.CreatedAt, b.ID} })
	return paginate(out, func(b LeaveBalance) cursorKey { return cursorKey{b.CreatedAt, b.ID} }, p)
}

func (s *MemoryStore) RecalculateLeaveBalance(ctx context.Context, orgID, employeeID, policyID string, asOf time.Time, holidays []Holiday) (LeaveBalance, error) {
	if err := ctx.Err(); err != nil {
		return LeaveBalance{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pol, ok := s.leavePolicies[gkey(orgID, policyID)]
	if !ok {
		return LeaveBalance{}, fmt.Errorf("%w: time-off policy %s", ErrNotFound, policyID)
	}
	emp, ok := s.employees[gkey(orgID, employeeID)]
	if !ok {
		return LeaveBalance{}, fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	requests := s.leaveRequestsForLocked(orgID, employeeID, policyID, "")
	bal, _ := s.computeBalanceLocked(orgID, emp, pol, requests, holidays, asOf)
	key := bkey(orgID, employeeID, policyID, bal.Period)
	now := time.Now().UTC()
	if prev, exists := s.leaveBalances[key]; exists {
		bal.ID = prev.ID
		bal.CreatedAt = prev.CreatedAt
		bal.Version = prev.Version + 1
	} else {
		bal.ID = newID()
		bal.CreatedAt = now
		bal.Version = 1
	}
	bal.UpdatedAt = now
	s.leaveBalances[key] = bal
	return copyLeaveBalance(bal), nil
}

// ---------------------------------------------------------------------------
// Leave requests
// ---------------------------------------------------------------------------

// snapshotLocked freezes the policy rules and the balance at request time.
// Callers must hold s.mu.
func snapshotLocked(pol TimeOffPolicy, bal LeaveBalance) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"policy": map[string]any{
			"id":                pol.ID,
			"name":              pol.Name,
			"accrual_mode":      string(pol.AccrualMode),
			"accrual_amount":    pol.AccrualAmount,
			"carry_forward_cap": pol.CarryForwardCap,
			"expiry_months":     pol.ExpiryMonths,
			"version":           pol.Version,
		},
		"balance_at_request": map[string]any{
			"period":   bal.Period,
			"accrued":  bal.Accrued,
			"taken":    bal.Taken,
			"carried":  bal.Carried,
			"expired":  bal.Expired,
			"balance":  bal.Balance,
			"computed": bal.ComputedAt,
		},
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// validateApproversLocked dedupes approver IDs and checks each names a
// joined employee in the organization. Callers must hold s.mu (read is fine
// under the write lock).
func (s *MemoryStore) validateApproversLocked(orgID string, ids []string) ([]string, error) {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		emp, ok := s.employees[gkey(orgID, id)]
		if !ok {
			return nil, &ValidationError{Field: "approver_ids", Message: "unknown employee " + id}
		}
		if emp.Status != InvitationJoined {
			return nil, &ValidationError{Field: "approver_ids", Message: "employee " + id + " is not joined"}
		}
		out = append(out, id)
	}
	return out, nil
}

func (s *MemoryStore) CreateTimeOffRequest(ctx context.Context, r TimeOffRequest, holidays []Holiday) (TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, err
	}
	if err := r.Validate(); err != nil {
		return TimeOffRequest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pol, ok := s.leavePolicies[gkey(r.OrganizationID, r.PolicyID)]
	if !ok {
		return TimeOffRequest{}, fmt.Errorf("%w: time-off policy %s", ErrNotFound, r.PolicyID)
	}
	emp, ok := s.employees[gkey(r.OrganizationID, r.EmployeeID)]
	if !ok {
		return TimeOffRequest{}, fmt.Errorf("%w: employee %s", ErrNotFound, r.EmployeeID)
	}
	if r.HolidayCalendarID != nil {
		if _, ok := s.holidayCalendars[gkey(r.OrganizationID, *r.HolidayCalendarID)]; !ok {
			return TimeOffRequest{}, fmt.Errorf("%w: holiday calendar %s", ErrNotFound, *r.HolidayCalendarID)
		}
	}
	start, end := r.startTime(), r.endTime()
	r.Days = WorkingDays(start, end, holidays)
	if r.Days <= 0 {
		return TimeOffRequest{}, &ValidationError{Field: "start_date", Message: "request covers no working days"}
	}
	asOf := start
	if !r.IsUnpaid {
		// Sufficiency check against the balance as it stands without this request.
		existing := s.leaveRequestsForLocked(r.OrganizationID, r.EmployeeID, r.PolicyID, "")
		_, raw := s.computeBalanceLocked(r.OrganizationID, emp, pol, existing, holidays, asOf)
		if r.Days > raw+1e-9 {
			return TimeOffRequest{}, fmt.Errorf("%w: need %.2f days, have %.2f", ErrInsufficientBalance, r.Days, raw)
		}
	}
	approvers, err := s.validateApproversLocked(r.OrganizationID, r.ApproverIDs)
	if err != nil {
		return TimeOffRequest{}, err
	}
	if r.ID == "" {
		r.ID = newID()
	}
	now := time.Now().UTC()
	r.CreatedAt = now
	r.UpdatedAt = now
	r.Version = 1
	r.Status = TimeOffRequested
	// Snapshot the balance including this request for auditability.
	withCandidate := s.leaveRequestsForLocked(r.OrganizationID, r.EmployeeID, r.PolicyID, "")
	withCandidate = append(withCandidate, r)
	bal, _ := s.computeBalanceLocked(r.OrganizationID, emp, pol, withCandidate, holidays, asOf)
	r.Snapshot = snapshotLocked(pol, bal)
	r.ApproverIDs = nil // write-only: the chain steps are the persisted form
	s.leaveRequests[gkey(r.OrganizationID, r.ID)] = r
	for i, aid := range approvers {
		step := TimeOffApproval{
			ID:                 newID(),
			OrganizationID:     r.OrganizationID,
			RequestID:          r.ID,
			Sequence:           i + 1,
			ApproverEmployeeID: aid,
			Status:             ApprovalPending,
			CreatedAt:          now,
		}
		s.leaveApprovals[gkey(r.OrganizationID, step.ID)] = step
	}
	return copyTimeOffRequest(r), nil
}

func (s *MemoryStore) GetTimeOffRequest(ctx context.Context, orgID, id string) (TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.leaveRequests[gkey(orgID, id)]
	if !ok {
		return TimeOffRequest{}, fmt.Errorf("%w: time-off request %s", ErrNotFound, id)
	}
	return copyTimeOffRequest(r), nil
}

func (s *MemoryStore) UpdateTimeOffRequest(ctx context.Context, r TimeOffRequest, holidays []Holiday) (TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, err
	}
	if err := r.Validate(); err != nil {
		return TimeOffRequest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.leaveRequests[gkey(r.OrganizationID, r.ID)]
	if !ok {
		return TimeOffRequest{}, fmt.Errorf("%w: time-off request %s", ErrNotFound, r.ID)
	}
	if r.Version != cur.Version {
		return TimeOffRequest{}, fmt.Errorf("%w: time-off request %s", ErrVersionConflict, r.ID)
	}
	if cur.Status != TimeOffRequested {
		return TimeOffRequest{}, fmt.Errorf("%w: cannot edit a %s request", ErrInvalidTransition, cur.Status)
	}
	pol, ok := s.leavePolicies[gkey(r.OrganizationID, r.PolicyID)]
	if !ok {
		return TimeOffRequest{}, fmt.Errorf("%w: time-off policy %s", ErrNotFound, r.PolicyID)
	}
	emp, ok := s.employees[gkey(r.OrganizationID, r.EmployeeID)]
	if !ok {
		return TimeOffRequest{}, fmt.Errorf("%w: employee %s", ErrNotFound, r.EmployeeID)
	}
	if r.HolidayCalendarID != nil {
		if _, ok := s.holidayCalendars[gkey(r.OrganizationID, *r.HolidayCalendarID)]; !ok {
			return TimeOffRequest{}, fmt.Errorf("%w: holiday calendar %s", ErrNotFound, *r.HolidayCalendarID)
		}
	}
	start, end := r.startTime(), r.endTime()
	days := WorkingDays(start, end, holidays)
	if days <= 0 {
		return TimeOffRequest{}, &ValidationError{Field: "start_date", Message: "request covers no working days"}
	}
	if !r.IsUnpaid {
		existing := s.leaveRequestsForLocked(r.OrganizationID, r.EmployeeID, r.PolicyID, r.ID)
		_, raw := s.computeBalanceLocked(r.OrganizationID, emp, pol, existing, holidays, start)
		if days > raw+1e-9 {
			return TimeOffRequest{}, fmt.Errorf("%w: need %.2f days, have %.2f", ErrInsufficientBalance, days, raw)
		}
	}
	cur.StartDate = r.StartDate
	cur.EndDate = r.EndDate
	cur.Days = days
	cur.IsUnpaid = r.IsUnpaid
	cur.HolidayCalendarID = copyStrPtr(r.HolidayCalendarID)
	cur.PolicyID = r.PolicyID
	cur.EmployeeID = r.EmployeeID
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.leaveRequests[gkey(r.OrganizationID, r.ID)] = cur
	return copyTimeOffRequest(cur), nil
}

func (s *MemoryStore) CancelTimeOffRequest(ctx context.Context, orgID, id string, version int64) (TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.leaveRequests[gkey(orgID, id)]
	if !ok {
		return TimeOffRequest{}, fmt.Errorf("%w: time-off request %s", ErrNotFound, id)
	}
	if version != cur.Version {
		return TimeOffRequest{}, fmt.Errorf("%w: time-off request %s", ErrVersionConflict, id)
	}
	if cur.Status != TimeOffRequested && cur.Status != TimeOffApproved {
		return TimeOffRequest{}, fmt.Errorf("%w: cannot cancel a %s request", ErrInvalidTransition, cur.Status)
	}
	cur.Status = TimeOffCancelled
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.leaveRequests[gkey(orgID, id)] = cur
	return copyTimeOffRequest(cur), nil
}

func (s *MemoryStore) ListTimeOffRequests(ctx context.Context, orgID string, f TimeOffRequestFilter, p Page) ([]TimeOffRequest, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TimeOffRequest, 0)
	for _, r := range s.leaveRequests {
		if r.OrganizationID != orgID {
			continue
		}
		if f.EmployeeID != "" && r.EmployeeID != f.EmployeeID {
			continue
		}
		if f.PolicyID != "" && r.PolicyID != f.PolicyID {
			continue
		}
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		out = append(out, copyTimeOffRequest(r))
	}
	sortByCursor(out, func(r TimeOffRequest) cursorKey { return cursorKey{r.CreatedAt, r.ID} })
	return paginate(out, func(r TimeOffRequest) cursorKey { return cursorKey{r.CreatedAt, r.ID} }, p)
}

func (s *MemoryStore) ListApprovedLeaveOnDate(ctx context.Context, orgID, date string, p Page) ([]TimeOffRequest, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if _, err := parseLeaveDate(date); err != nil {
		return nil, "", &ValidationError{Field: "date", Message: "must be YYYY-MM-DD"}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]TimeOffRequest, 0)
	for _, r := range s.leaveRequests {
		if r.OrganizationID != orgID || r.Status != TimeOffApproved {
			continue
		}
		if r.StartDate <= date && date <= r.EndDate {
			out = append(out, copyTimeOffRequest(r))
		}
	}
	sortByCursor(out, func(r TimeOffRequest) cursorKey { return cursorKey{r.CreatedAt, r.ID} })
	return paginate(out, func(r TimeOffRequest) cursorKey { return cursorKey{r.CreatedAt, r.ID} }, p)
}

// ---------------------------------------------------------------------------
// Approval chains
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateTimeOffApproval(ctx context.Context, a TimeOffApproval) (TimeOffApproval, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffApproval{}, err
	}
	if err := a.Validate(); err != nil {
		return TimeOffApproval{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.leaveRequests[gkey(a.OrganizationID, a.RequestID)]
	if !ok {
		return TimeOffApproval{}, fmt.Errorf("%w: time-off request %s", ErrNotFound, a.RequestID)
	}
	if req.Status != TimeOffRequested {
		return TimeOffApproval{}, fmt.Errorf("%w: cannot extend the chain of a %s request", ErrInvalidTransition, req.Status)
	}
	emp, ok := s.employees[gkey(a.OrganizationID, a.ApproverEmployeeID)]
	if !ok {
		return TimeOffApproval{}, fmt.Errorf("%w: approver employee %s", ErrNotFound, a.ApproverEmployeeID)
	}
	if emp.Status != InvitationJoined {
		return TimeOffApproval{}, &ValidationError{Field: "approver_employee_id", Message: "employee is not joined"}
	}
	maxSeq := 0
	for _, step := range s.leaveApprovals {
		if step.OrganizationID == a.OrganizationID && step.RequestID == a.RequestID && step.Sequence > maxSeq {
			maxSeq = step.Sequence
		}
	}
	if a.Sequence <= 0 {
		a.Sequence = maxSeq + 1
	} else {
		for _, step := range s.leaveApprovals {
			if step.OrganizationID == a.OrganizationID && step.RequestID == a.RequestID && step.Sequence == a.Sequence {
				return TimeOffApproval{}, fmt.Errorf("%w: approval sequence %d", ErrAlreadyExists, a.Sequence)
			}
		}
	}
	if a.ID == "" {
		a.ID = newID()
	}
	a.Status = ApprovalPending
	a.DecidedBy = nil
	a.DecidedAt = nil
	a.CreatedAt = time.Now().UTC()
	s.leaveApprovals[gkey(a.OrganizationID, a.ID)] = a
	return copyTimeOffApproval(a), nil
}

func (s *MemoryStore) ListTimeOffApprovals(ctx context.Context, orgID, requestID string, p Page) ([]TimeOffApproval, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.leaveRequests[gkey(orgID, requestID)]; !ok {
		return nil, "", fmt.Errorf("%w: time-off request %s", ErrNotFound, requestID)
	}
	out := make([]TimeOffApproval, 0)
	for _, a := range s.leaveApprovals {
		if a.OrganizationID == orgID && a.RequestID == requestID {
			out = append(out, copyTimeOffApproval(a))
		}
	}
	// Sequence order for display; steps are created in sequence order so the
	// (created_at, id) cursor stays consistent with it.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Sequence != out[j].Sequence {
			return out[i].Sequence < out[j].Sequence
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return paginate(out, func(a TimeOffApproval) cursorKey { return cursorKey{a.CreatedAt, a.ID} }, p)
}

// approvalsForLocked returns the request's steps sorted by sequence.
// Callers must hold s.mu.
func (s *MemoryStore) approvalsForLocked(orgID, requestID string) []TimeOffApproval {
	var out []TimeOffApproval
	for _, a := range s.leaveApprovals {
		if a.OrganizationID == orgID && a.RequestID == requestID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

// currentPendingLocked returns the lowest-sequence pending step, or nil.
// Callers must hold s.mu.
func currentPendingLocked(steps []TimeOffApproval) *TimeOffApproval {
	for i := range steps {
		if steps[i].Status == ApprovalPending {
			return &steps[i]
		}
	}
	return nil
}

// decideLocked applies one approval decision. Callers must hold s.mu and
// have loaded the request.
func (s *MemoryStore) decideLocked(req *TimeOffRequest, steps []TimeOffApproval, approverEmployeeID string, approved bool, asAdmin bool) error {
	step, newStatus, err := resolveDecision(req, steps, approverEmployeeID, approved, asAdmin)
	if err != nil {
		return err
	}
	if step != nil {
		s.leaveApprovals[gkey(req.OrganizationID, step.ID)] = *step
	}
	req.Status = newStatus
	req.UpdatedAt = time.Now().UTC()
	req.Version++
	s.leaveRequests[gkey(req.OrganizationID, req.ID)] = *req
	return nil
}

func (s *MemoryStore) DecideTimeOffApproval(ctx context.Context, orgID, requestID, approverEmployeeID string, approved bool, requestVersion int64, asAdmin bool) (TimeOffRequest, []TimeOffApproval, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.leaveRequests[gkey(orgID, requestID)]
	if !ok {
		return TimeOffRequest{}, nil, fmt.Errorf("%w: time-off request %s", ErrNotFound, requestID)
	}
	if requestVersion != req.Version {
		return TimeOffRequest{}, nil, fmt.Errorf("%w: time-off request %s", ErrVersionConflict, requestID)
	}
	steps := s.approvalsForLocked(orgID, requestID)
	if err := s.decideLocked(&req, steps, approverEmployeeID, approved, asAdmin); err != nil {
		return TimeOffRequest{}, nil, err
	}
	out := s.approvalsForLocked(orgID, requestID)
	for i := range out {
		out[i] = copyTimeOffApproval(out[i])
	}
	return copyTimeOffRequest(req), out, nil
}

func (s *MemoryStore) BulkApproveTimeOffRequests(ctx context.Context, orgID string, ids []string, approverEmployeeID string, asAdmin bool) ([]TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, &ValidationError{Field: "ids", Message: "at least one request id is required"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	type candidate struct {
		req   TimeOffRequest
		steps []TimeOffApproval
	}
	cands := make([]candidate, 0, len(ids))
	results := make([]BulkApprovalResult, 0, len(ids))
	failed := false
	for _, id := range ids {
		res := BulkApprovalResult{RequestID: id}
		req, ok := s.leaveRequests[gkey(orgID, id)]
		if !ok {
			res.Reason = "not found"
		} else if req.Status != TimeOffRequested {
			res.Reason = "request is " + string(req.Status)
		} else {
			steps := s.approvalsForLocked(orgID, id)
			cur := currentPendingLocked(steps)
			switch {
			case cur == nil && !asAdmin:
				res.Reason = "no pending approval step"
			case cur != nil && cur.ApproverEmployeeID != approverEmployeeID && !asAdmin:
				res.Reason = "not the current approver"
			default:
				res.Approved = true
				cands = append(cands, candidate{req: req, steps: steps})
			}
		}
		if !res.Approved {
			failed = true
		}
		results = append(results, res)
	}
	if failed {
		return nil, &BulkApprovalError{Results: results}
	}
	// All validated: apply. Nothing above mutated, so this is atomic under
	// the single write lock.
	out := make([]TimeOffRequest, 0, len(cands))
	for _, c := range cands {
		req := c.req
		if err := s.decideLocked(&req, c.steps, approverEmployeeID, true, asAdmin); err != nil {
			// Unreachable: validated above under the same lock.
			return nil, err
		}
		out = append(out, copyTimeOffRequest(req))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Holiday calendars and holidays
// ---------------------------------------------------------------------------

func (s *MemoryStore) CreateHolidayCalendar(ctx context.Context, c HolidayCalendar) (HolidayCalendar, error) {
	if err := ctx.Err(); err != nil {
		return HolidayCalendar{}, err
	}
	if err := c.Validate(); err != nil {
		return HolidayCalendar{}, err
	}
	if c.ID == "" {
		c.ID = newID()
	}
	now := time.Now().UTC()
	c.CreatedAt = now
	c.UpdatedAt = now
	c.Version = 1

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[c.OrganizationID]; !ok {
		return HolidayCalendar{}, fmt.Errorf("%w: organization %s", ErrNotFound, c.OrganizationID)
	}
	if _, ok := s.holidayCalendars[gkey(c.OrganizationID, c.ID)]; ok {
		return HolidayCalendar{}, fmt.Errorf("%w: holiday calendar %s", ErrAlreadyExists, c.ID)
	}
	s.holidayCalendars[gkey(c.OrganizationID, c.ID)] = c
	return copyHolidayCalendar(c), nil
}

func (s *MemoryStore) GetHolidayCalendar(ctx context.Context, orgID, id string) (HolidayCalendar, error) {
	if err := ctx.Err(); err != nil {
		return HolidayCalendar{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.holidayCalendars[gkey(orgID, id)]
	if !ok {
		return HolidayCalendar{}, fmt.Errorf("%w: holiday calendar %s", ErrNotFound, id)
	}
	return copyHolidayCalendar(c), nil
}

func (s *MemoryStore) UpdateHolidayCalendar(ctx context.Context, c HolidayCalendar) (HolidayCalendar, error) {
	if err := ctx.Err(); err != nil {
		return HolidayCalendar{}, err
	}
	if err := c.Validate(); err != nil {
		return HolidayCalendar{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.holidayCalendars[gkey(c.OrganizationID, c.ID)]
	if !ok {
		return HolidayCalendar{}, fmt.Errorf("%w: holiday calendar %s", ErrNotFound, c.ID)
	}
	if c.Version != cur.Version {
		return HolidayCalendar{}, fmt.Errorf("%w: holiday calendar %s", ErrVersionConflict, c.ID)
	}
	cur.Name = c.Name
	cur.CountryCode = c.CountryCode
	cur.UpdatedAt = time.Now().UTC()
	cur.Version++
	s.holidayCalendars[gkey(c.OrganizationID, c.ID)] = cur
	return copyHolidayCalendar(cur), nil
}

func (s *MemoryStore) DeleteHolidayCalendar(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.holidayCalendars[gkey(orgID, id)]; !ok {
		return fmt.Errorf("%w: holiday calendar %s", ErrNotFound, id)
	}
	for _, h := range s.holidays {
		if h.OrganizationID == orgID && h.CalendarID == id {
			return fmt.Errorf("%w: calendar %s still has holidays", ErrNotEmpty, id)
		}
	}
	delete(s.holidayCalendars, gkey(orgID, id))
	return nil
}

func (s *MemoryStore) ListHolidayCalendars(ctx context.Context, orgID string, p Page) ([]HolidayCalendar, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]HolidayCalendar, 0)
	for _, c := range s.holidayCalendars {
		if c.OrganizationID == orgID {
			out = append(out, copyHolidayCalendar(c))
		}
	}
	sortByCursor(out, func(c HolidayCalendar) cursorKey { return cursorKey{c.CreatedAt, c.ID} })
	return paginate(out, func(c HolidayCalendar) cursorKey { return cursorKey{c.CreatedAt, c.ID} }, p)
}

func (s *MemoryStore) CreateHoliday(ctx context.Context, h Holiday) (Holiday, error) {
	if err := ctx.Err(); err != nil {
		return Holiday{}, err
	}
	if err := h.Validate(); err != nil {
		return Holiday{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.holidayCalendars[gkey(h.OrganizationID, h.CalendarID)]; !ok {
		return Holiday{}, fmt.Errorf("%w: holiday calendar %s", ErrNotFound, h.CalendarID)
	}
	for _, ex := range s.holidays {
		if ex.OrganizationID == h.OrganizationID && ex.CalendarID == h.CalendarID && ex.Date == h.Date {
			return Holiday{}, fmt.Errorf("%w: holiday on %s", ErrAlreadyExists, h.Date)
		}
	}
	if h.ID == "" {
		h.ID = newID()
	}
	h.CreatedAt = time.Now().UTC()
	s.holidays[gkey(h.OrganizationID, h.ID)] = h
	return copyHoliday(h), nil
}

func (s *MemoryStore) GetHoliday(ctx context.Context, orgID, id string) (Holiday, error) {
	if err := ctx.Err(); err != nil {
		return Holiday{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.holidays[gkey(orgID, id)]
	if !ok {
		return Holiday{}, fmt.Errorf("%w: holiday %s", ErrNotFound, id)
	}
	return copyHoliday(h), nil
}

func (s *MemoryStore) DeleteHoliday(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.holidays[gkey(orgID, id)]; !ok {
		return fmt.Errorf("%w: holiday %s", ErrNotFound, id)
	}
	delete(s.holidays, gkey(orgID, id))
	return nil
}

func (s *MemoryStore) ListHolidays(ctx context.Context, orgID, calendarID string, p Page) ([]Holiday, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.holidayCalendars[gkey(orgID, calendarID)]; !ok {
		return nil, "", fmt.Errorf("%w: holiday calendar %s", ErrNotFound, calendarID)
	}
	out := make([]Holiday, 0)
	for _, h := range s.holidays {
		if h.OrganizationID == orgID && h.CalendarID == calendarID {
			out = append(out, copyHoliday(h))
		}
	}
	// Date order for display; the cursor keys on (date, id).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Date != out[j].Date {
			return out[i].Date < out[j].Date
		}
		return out[i].ID < out[j].ID
	})
	return paginate(out, func(h Holiday) cursorKey { return cursorKey{h.dateTime(), h.ID} }, p)
}
