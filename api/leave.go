// Package api — leave (component 3): time-off policies, leave requests with
// ordered approval chains, the balance ledger, and holiday calendars.
//
// The leave-domain sentinels are mapped by the shared mapError in server.go:
// ErrInsufficientBalance -> 409 INSUFFICIENT_BALANCE,
// ErrNotCurrentApprover -> 409 NOT_CURRENT_APPROVER, ErrInvalidTransition ->
// 409 INVALID_TRANSITION, and *BulkApprovalError -> 409 BULK_APPROVAL_FAILED
// with per-request results.
package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// leaveRoutes registers the component 3 routes. Worker C calls it from
// routes().
func (s *Server) leaveRoutes() {
	m := s.mux
	base := "/api/v1/organizations/{orgID}"

	// Time-off policies.
	m.HandleFunc("POST "+base+"/leave-policies",
		s.withAudit("leave_policy.created", "leave_policy", s.createLeavePolicy))
	m.HandleFunc("GET "+base+"/leave-policies", s.handleListLeavePolicies)
	m.HandleFunc("GET "+base+"/leave-policies/{policyID}", s.handleGetLeavePolicy)
	m.HandleFunc("PATCH "+base+"/leave-policies/{policyID}",
		s.withAudit("leave_policy.updated", "leave_policy", s.patchLeavePolicy))
	m.HandleFunc("DELETE "+base+"/leave-policies/{policyID}",
		s.withAudit("leave_policy.deleted", "leave_policy", s.deleteLeavePolicy))

	// Leave balances.
	m.HandleFunc("GET "+base+"/leave-balances", s.handleListLeaveBalances)
	m.HandleFunc("POST "+base+"/leave-balances/recalculate",
		s.withAudit("leave_balance.recalculated", "leave_balance", s.recalculateLeaveBalance))

	// Leave requests. The literal bulk-approve segment wins over {id} in
	// Go's ServeMux.
	m.HandleFunc("POST "+base+"/leave-requests/bulk-approve",
		s.withAudit("leave_request.bulk_approved", "leave_request", s.bulkApproveLeaveRequests))
	m.HandleFunc("POST "+base+"/leave-requests",
		s.withAudit("leave_request.created", "leave_request", s.createLeaveRequest))
	m.HandleFunc("GET "+base+"/leave-requests", s.handleListLeaveRequests)
	m.HandleFunc("GET "+base+"/leave-requests/{id}", s.handleGetLeaveRequest)
	m.HandleFunc("PATCH "+base+"/leave-requests/{id}",
		s.withAudit("leave_request.updated", "leave_request", s.patchLeaveRequest))
	m.HandleFunc("POST "+base+"/leave-requests/{id}/cancel",
		s.withAudit("leave_request.cancelled", "leave_request", s.cancelLeaveRequest))
	m.HandleFunc("POST "+base+"/leave-requests/{id}/approve",
		s.withAudit("leave_request.approved", "leave_request", s.approveLeaveRequest))
	m.HandleFunc("POST "+base+"/leave-requests/{id}/reject",
		s.withAudit("leave_request.rejected", "leave_request", s.rejectLeaveRequest))
	m.HandleFunc("GET "+base+"/leave-requests/{id}/approvals", s.handleListLeaveApprovals)
	m.HandleFunc("POST "+base+"/leave-requests/{id}/approvals",
		s.withAudit("leave_approval.created", "leave_approval", s.addLeaveApproval))

	// Holiday calendars and holidays.
	m.HandleFunc("POST "+base+"/holiday-calendars",
		s.withAudit("holiday_calendar.created", "holiday_calendar", s.createHolidayCalendar))
	m.HandleFunc("GET "+base+"/holiday-calendars", s.handleListHolidayCalendars)
	m.HandleFunc("GET "+base+"/holiday-calendars/{calendarID}", s.handleGetHolidayCalendar)
	m.HandleFunc("PATCH "+base+"/holiday-calendars/{calendarID}",
		s.withAudit("holiday_calendar.updated", "holiday_calendar", s.patchHolidayCalendar))
	m.HandleFunc("DELETE "+base+"/holiday-calendars/{calendarID}",
		s.withAudit("holiday_calendar.deleted", "holiday_calendar", s.deleteHolidayCalendar))
	m.HandleFunc("POST "+base+"/holiday-calendars/{calendarID}/holidays",
		s.withAudit("holiday.created", "holiday", s.createHoliday))
	m.HandleFunc("GET "+base+"/holiday-calendars/{calendarID}/holidays", s.handleListHolidays)
	m.HandleFunc("DELETE "+base+"/holiday-calendars/{calendarID}/holidays/{holidayID}",
		s.withAudit("holiday.deleted", "holiday", s.deleteHoliday))

	// Who is on leave.
	m.HandleFunc("GET "+base+"/who-is-on-leave", s.handleWhoIsOnLeave)
}

// leaveStoreOf returns the store's LeaveStore facet. Both bundled store
// implementations provide it; the facet is embedded in Store.
func (s *Server) leaveStoreOf(w http.ResponseWriter, r *http.Request) (store.LeaveStore, bool) {
	ls, ok := s.store.(store.LeaveStore)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "leave store not available")
		return nil, false
	}
	return ls, true
}

// leaveSelfOrManager allows the employee themselves, or anyone with the
// manager role or higher.
func leaveSelfOrManager(w http.ResponseWriter, r *http.Request, ai *AuthInfo, employeeID, what string) bool {
	if ai.EmployeeID != nil && *ai.EmployeeID == employeeID {
		return true
	}
	return requireRoleOr(w, r, ai, store.RoleManager, what)
}

// holidaysForCalendar loads every holiday of a calendar, paging internally.
func holidaysForCalendar(w http.ResponseWriter, r *http.Request, ls store.LeaveStore, orgID, calendarID string) ([]store.Holiday, bool) {
	ctx := r.Context()
	var all []store.Holiday
	cursor := ""
	for {
		page, next, err := ls.ListHolidays(ctx, orgID, calendarID, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return nil, false
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return all, true
}

// --- time-off policies ---

type createLeavePolicyRequest struct {
	Name            string  `json:"name"`
	AccrualMode     string  `json:"accrual_mode"`
	AccrualAmount   float64 `json:"accrual_amount"`
	CarryForwardCap float64 `json:"carry_forward_cap"`
	ExpiryMonths    int     `json:"expiry_months"`
}

func (s *Server) createLeavePolicy(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage leave policies") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req createLeavePolicyRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	created, err := ls.CreateTimeOffPolicy(r.Context(), store.TimeOffPolicy{
		OrganizationID:  orgID,
		Name:            req.Name,
		AccrualMode:     store.AccrualMode(req.AccrualMode),
		AccrualAmount:   req.AccrualAmount,
		CarryForwardCap: req.CarryForwardCap,
		ExpiryMonths:    req.ExpiryMonths,
	})
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListLeavePolicies(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	policies, next, err := ls.ListTimeOffPolicies(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	if policies == nil {
		policies = []store.TimeOffPolicy{}
	}
	writePage(w, policies, next)
}

func (s *Server) handleGetLeavePolicy(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	pol, err := ls.GetTimeOffPolicy(r.Context(), orgID, r.PathValue("policyID"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, pol)
}

type patchLeavePolicyRequest struct {
	Name            *string  `json:"name,omitempty"`
	AccrualMode     *string  `json:"accrual_mode,omitempty"`
	AccrualAmount   *float64 `json:"accrual_amount,omitempty"`
	CarryForwardCap *float64 `json:"carry_forward_cap,omitempty"`
	ExpiryMonths    *int     `json:"expiry_months,omitempty"`
	Version         int64    `json:"version"`
}

func (s *Server) patchLeavePolicy(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage leave policies") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req patchLeavePolicyRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := ls.GetTimeOffPolicy(r.Context(), orgID, r.PathValue("policyID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.Name != nil {
		cur.Name = *req.Name
	}
	if req.AccrualMode != nil {
		cur.AccrualMode = store.AccrualMode(*req.AccrualMode)
	}
	if req.AccrualAmount != nil {
		cur.AccrualAmount = *req.AccrualAmount
	}
	if req.CarryForwardCap != nil {
		cur.CarryForwardCap = *req.CarryForwardCap
	}
	if req.ExpiryMonths != nil {
		cur.ExpiryMonths = *req.ExpiryMonths
	}
	updated, err := ls.UpdateTimeOffPolicy(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) deleteLeavePolicy(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage leave policies") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	policyID := r.PathValue("policyID")
	before, err := ls.GetTimeOffPolicy(r.Context(), orgID, policyID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if err := ls.DeleteTimeOffPolicy(r.Context(), orgID, policyID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: policyID, Before: before}, true
}

// --- leave balances ---

func (s *Server) handleListLeaveBalances(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	balances, next, err := ls.ListLeaveBalances(r.Context(), orgID, store.LeaveBalanceFilter{
		EmployeeID: q.Get("employee_id"),
		PolicyID:   q.Get("policy_id"),
		Period:     q.Get("period"),
	}, page)
	if !mapError(w, r, err) {
		return
	}
	if balances == nil {
		balances = []store.LeaveBalance{}
	}
	writePage(w, balances, next)
}

type recalculateBalanceRequest struct {
	EmployeeID        string  `json:"employee_id"`
	PolicyID          string  `json:"policy_id"`
	AsOf              *string `json:"as_of,omitempty"` // RFC3339; defaults to now
	HolidayCalendarID *string `json:"holiday_calendar_id,omitempty"`
}

func (s *Server) recalculateLeaveBalance(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req recalculateBalanceRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if strings.TrimSpace(req.EmployeeID) == "" || strings.TrimSpace(req.PolicyID) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "employee_id and policy_id are required")
		return zero, false
	}
	if !leaveSelfOrManager(w, r, ai, req.EmployeeID, " to recalculate this balance") {
		return zero, false
	}
	asOf := time.Now().UTC()
	if req.AsOf != nil && *req.AsOf != "" {
		t, err := time.Parse(time.RFC3339, *req.AsOf)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "as_of: must be RFC3339")
			return zero, false
		}
		asOf = t
	}
	var holidays []store.Holiday
	if req.HolidayCalendarID != nil && *req.HolidayCalendarID != "" {
		holidays, ok = holidaysForCalendar(w, r, ls, orgID, *req.HolidayCalendarID)
		if !ok {
			return zero, false
		}
	}
	before, _ := ls.GetLeaveBalance(r.Context(), orgID, req.EmployeeID, req.PolicyID, asOf.Format("2006"))
	bal, err := ls.RecalculateLeaveBalance(r.Context(), orgID, req.EmployeeID, req.PolicyID, asOf, holidays)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, bal)
	return auditData{ResourceID: bal.ID, Before: before, After: bal}, true
}

// --- leave requests ---

type createLeaveRequestBody struct {
	EmployeeID        string   `json:"employee_id"`
	PolicyID          string   `json:"policy_id"`
	StartDate         string   `json:"start_date"` // YYYY-MM-DD
	EndDate           string   `json:"end_date"`   // YYYY-MM-DD
	IsUnpaid          bool     `json:"is_unpaid"`
	HolidayCalendarID *string  `json:"holiday_calendar_id,omitempty"`
	ApproverIDs       []string `json:"approver_ids,omitempty"`
}

func (s *Server) createLeaveRequest(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveWrite) {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req createLeaveRequestBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if !leaveSelfOrManager(w, r, ai, req.EmployeeID, " to request leave for this employee") {
		return zero, false
	}
	if len(req.ApproverIDs) > 0 && !requireRole(store.RoleManager, ai) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN",
			"only managers and above can set an explicit approver chain")
		return zero, false
	}
	// Default chain: the employee's manager, when set. An explicit chain
	// overrides it. No manager and no explicit chain means the request can
	// only be decided by an admin.
	approvers := req.ApproverIDs
	if len(approvers) == 0 {
		emp, err := s.store.GetEmployee(r.Context(), orgID, req.EmployeeID)
		if !mapError(w, r, err) {
			return zero, false
		}
		if emp.ManagerID != nil && *emp.ManagerID != "" {
			approvers = []string{*emp.ManagerID}
		}
	}
	var holidays []store.Holiday
	if req.HolidayCalendarID != nil && *req.HolidayCalendarID != "" {
		holidays, ok = holidaysForCalendar(w, r, ls, orgID, *req.HolidayCalendarID)
		if !ok {
			return zero, false
		}
	}
	created, err := ls.CreateTimeOffRequest(r.Context(), store.TimeOffRequest{
		OrganizationID:    orgID,
		EmployeeID:        req.EmployeeID,
		PolicyID:          req.PolicyID,
		HolidayCalendarID: derefOrEmpty(req.HolidayCalendarID),
		StartDate:         req.StartDate,
		EndDate:           req.EndDate,
		IsUnpaid:          req.IsUnpaid,
		ApproverIDs:       approvers,
	}, holidays)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListLeaveRequests(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	status := store.TimeOffStatus(q.Get("status"))
	if status != "" && !store.ValidTimeOffStatus(status) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "status: unknown time-off status")
		return
	}
	requests, next, err := ls.ListTimeOffRequests(r.Context(), orgID, store.TimeOffRequestFilter{
		EmployeeID: q.Get("employee_id"),
		PolicyID:   q.Get("policy_id"),
		Status:     status,
	}, page)
	if !mapError(w, r, err) {
		return
	}
	if requests == nil {
		requests = []store.TimeOffRequest{}
	}
	writePage(w, requests, next)
}

func (s *Server) handleGetLeaveRequest(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	req, err := ls.GetTimeOffRequest(r.Context(), orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, req)
}

type patchLeaveRequestBody struct {
	StartDate         *string `json:"start_date,omitempty"`
	EndDate           *string `json:"end_date,omitempty"`
	IsUnpaid          *bool   `json:"is_unpaid,omitempty"`
	HolidayCalendarID *string `json:"holiday_calendar_id,omitempty"`
	Version           int64   `json:"version"`
}

func (s *Server) patchLeaveRequest(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveWrite) {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req patchLeaveRequestBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := ls.GetTimeOffRequest(r.Context(), orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return zero, false
	}
	if !leaveSelfOrManager(w, r, ai, cur.EmployeeID, " to edit this leave request") {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.StartDate != nil {
		cur.StartDate = *req.StartDate
	}
	if req.EndDate != nil {
		cur.EndDate = *req.EndDate
	}
	if req.IsUnpaid != nil {
		cur.IsUnpaid = *req.IsUnpaid
	}
	if req.HolidayCalendarID != nil {
		cur.HolidayCalendarID = derefOrEmpty(req.HolidayCalendarID)
	}
	var holidays []store.Holiday
	if cur.HolidayCalendarID != nil {
		holidays, ok = holidaysForCalendar(w, r, ls, orgID, *cur.HolidayCalendarID)
		if !ok {
			return zero, false
		}
	}
	updated, err := ls.UpdateTimeOffRequest(r.Context(), cur, holidays)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

type versionBody struct {
	Version int64 `json:"version"`
}

func (s *Server) cancelLeaveRequest(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveWrite) {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req versionBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := ls.GetTimeOffRequest(r.Context(), orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return zero, false
	}
	if !leaveSelfOrManager(w, r, ai, cur.EmployeeID, " to cancel this leave request") {
		return zero, false
	}
	cancelled, err := ls.CancelTimeOffRequest(r.Context(), orgID, cur.ID, req.Version)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, cancelled)
	return auditData{ResourceID: cancelled.ID, Before: cur, After: cancelled}, true
}

// decideLeaveRequest backs both the approve and reject endpoints.
func (s *Server) decideLeaveRequest(w http.ResponseWriter, r *http.Request, ai *AuthInfo, approved bool) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveApprove) {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req versionBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	before, err := ls.GetTimeOffRequest(r.Context(), orgID, r.PathValue("id"))
	if !mapError(w, r, err) {
		return zero, false
	}
	var approverID string
	if ai.EmployeeID != nil {
		approverID = *ai.EmployeeID
	}
	asAdmin := requireRole(store.RoleAdmin, ai)
	updated, steps, err := ls.DecideTimeOffApproval(r.Context(), orgID, before.ID, approverID, approved, req.Version, asAdmin)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"request":   updated,
		"approvals": steps,
	})
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) approveLeaveRequest(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.decideLeaveRequest(w, r, ai, true)
}

func (s *Server) rejectLeaveRequest(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.decideLeaveRequest(w, r, ai, false)
}

type bulkApproveBody struct {
	IDs     []string `json:"ids"`
	Preview bool     `json:"preview"`
}

type bulkApprovePreview struct {
	RequestID       string  `json:"request_id"`
	CanApprove      bool    `json:"can_approve"`
	Reason          string  `json:"reason,omitempty"`
	CurrentApprover *string `json:"current_approver,omitempty"`
	Status          string  `json:"status,omitempty"`
}

// bulkApproveLeaveRequests implements the two-phase bulk approval: preview
// reports per-request approvability without mutating; confirm approves all
// listed requests atomically (all-or-nothing). The confirm is idempotent via
// the standard Idempotency-Key middleware.
func (s *Server) bulkApproveLeaveRequests(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveApprove) {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req bulkApproveBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if len(req.IDs) == 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "ids: at least one request id is required")
		return zero, false
	}
	var approverID string
	if ai.EmployeeID != nil {
		approverID = *ai.EmployeeID
	}
	asAdmin := requireRole(store.RoleAdmin, ai)
	ctx := r.Context()

	if req.Preview {
		previews := make([]bulkApprovePreview, 0, len(req.IDs))
		for _, id := range req.IDs {
			pv := bulkApprovePreview{RequestID: id}
			lr, err := ls.GetTimeOffRequest(ctx, orgID, id)
			if err != nil {
				pv.Reason = "not found"
				previews = append(previews, pv)
				continue
			}
			pv.Status = string(lr.Status)
			if lr.Status != store.TimeOffRequested {
				pv.Reason = "request is " + string(lr.Status)
				previews = append(previews, pv)
				continue
			}
			steps, _, err := ls.ListTimeOffApprovals(ctx, orgID, id, store.Page{Limit: 1000})
			if err != nil {
				pv.Reason = "could not load approval chain"
				previews = append(previews, pv)
				continue
			}
			var current *string
			for i := range steps {
				if steps[i].Status == store.ApprovalPending {
					c := steps[i].ApproverEmployeeID
					current = &c
					break
				}
			}
			pv.CurrentApprover = current
			switch {
			case current == nil && !asAdmin:
				pv.Reason = "no pending approval step"
			case current != nil && *current != approverID && !asAdmin:
				pv.Reason = "not the current approver"
			default:
				pv.CanApprove = true
			}
			previews = append(previews, pv)
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": previews})
		return zero, false // preview is read-only: no audit event
	}

	approved, err := ls.BulkApproveTimeOffRequests(ctx, orgID, req.IDs, approverID, asAdmin)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, map[string]any{"approved": approved})
	ids := make([]string, 0, len(approved))
	for _, a := range approved {
		ids = append(ids, a.ID)
	}
	return auditData{ResourceID: strings.Join(ids, ","), After: approved}, true
}

func (s *Server) handleListLeaveApprovals(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	steps, next, err := ls.ListTimeOffApprovals(r.Context(), orgID, r.PathValue("id"), page)
	if !mapError(w, r, err) {
		return
	}
	if steps == nil {
		steps = []store.TimeOffApproval{}
	}
	writePage(w, steps, next)
}

type addLeaveApprovalBody struct {
	ApproverEmployeeID string `json:"approver_employee_id"`
	Sequence           int    `json:"sequence,omitempty"` // <= 0 auto-assigns max+1
}

func (s *Server) addLeaveApproval(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage approval chains") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req addLeaveApprovalBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	created, err := ls.CreateTimeOffApproval(r.Context(), store.TimeOffApproval{
		OrganizationID:     orgID,
		RequestID:          r.PathValue("id"),
		Sequence:           req.Sequence,
		ApproverEmployeeID: req.ApproverEmployeeID,
	})
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

// --- holiday calendars and holidays ---

type createHolidayCalendarBody struct {
	Name        string `json:"name"`
	CountryCode string `json:"country_code"`
}

func (s *Server) createHolidayCalendar(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage holiday calendars") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req createHolidayCalendarBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	created, err := ls.CreateHolidayCalendar(r.Context(), store.HolidayCalendar{
		OrganizationID: orgID,
		Name:           req.Name,
		CountryCode:    req.CountryCode,
	})
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListHolidayCalendars(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	cals, next, err := ls.ListHolidayCalendars(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	if cals == nil {
		cals = []store.HolidayCalendar{}
	}
	writePage(w, cals, next)
}

func (s *Server) handleGetHolidayCalendar(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	cal, err := ls.GetHolidayCalendar(r.Context(), orgID, r.PathValue("calendarID"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, cal)
}

type patchHolidayCalendarBody struct {
	Name        *string `json:"name,omitempty"`
	CountryCode *string `json:"country_code,omitempty"`
	Version     int64   `json:"version"`
}

func (s *Server) patchHolidayCalendar(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage holiday calendars") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req patchHolidayCalendarBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := ls.GetHolidayCalendar(r.Context(), orgID, r.PathValue("calendarID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.Name != nil {
		cur.Name = *req.Name
	}
	if req.CountryCode != nil {
		cur.CountryCode = *req.CountryCode
	}
	updated, err := ls.UpdateHolidayCalendar(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) deleteHolidayCalendar(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage holiday calendars") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	calendarID := r.PathValue("calendarID")
	before, err := ls.GetHolidayCalendar(r.Context(), orgID, calendarID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if err := ls.DeleteHolidayCalendar(r.Context(), orgID, calendarID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: calendarID, Before: before}, true
}

type createHolidayBody struct {
	Date string `json:"date"` // YYYY-MM-DD
	Name string `json:"name"`
}

func (s *Server) createHoliday(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage holidays") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	var req createHolidayBody
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	created, err := ls.CreateHoliday(r.Context(), store.Holiday{
		OrganizationID: orgID,
		CalendarID:     r.PathValue("calendarID"),
		Date:           req.Date,
		Name:           req.Name,
	})
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListHolidays(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	holidays, next, err := ls.ListHolidays(r.Context(), orgID, r.PathValue("calendarID"), page)
	if !mapError(w, r, err) {
		return
	}
	if holidays == nil {
		holidays = []store.Holiday{}
	}
	writePage(w, holidays, next)
}

func (s *Server) deleteHoliday(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to manage holidays") {
		return zero, false
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return zero, false
	}
	holidayID := r.PathValue("holidayID")
	before, err := ls.GetHoliday(r.Context(), orgID, holidayID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if before.CalendarID != r.PathValue("calendarID") {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "holiday not found in this calendar")
		return zero, false
	}
	if err := ls.DeleteHoliday(r.Context(), orgID, holidayID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: holidayID, Before: before}, true
}

// --- who is on leave ---

type whoIsOnLeaveEntry struct {
	RequestID    string `json:"request_id"`
	EmployeeID   string `json:"employee_id"`
	EmployeeName string `json:"employee_name"`
	PolicyID     string `json:"policy_id"`
	PolicyName   string `json:"policy_name"`
	StartDate    string `json:"start_date"`
	EndDate      string `json:"end_date"`
	IsUnpaid     bool   `json:"is_unpaid"`
}

func (s *Server) handleWhoIsOnLeave(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeLeaveRead) {
		return
	}
	ls, ok := s.leaveStoreOf(w, r)
	if !ok {
		return
	}
	date := r.URL.Query().Get("date")
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	ctx := r.Context()
	var entries []whoIsOnLeaveEntry
	cursor := ""
	for {
		reqs, next, err := ls.ListApprovedLeaveOnDate(ctx, orgID, date, store.Page{Limit: 1000, Cursor: cursor})
		if !mapError(w, r, err) {
			return
		}
		for _, lr := range reqs {
			e := whoIsOnLeaveEntry{
				RequestID:  lr.ID,
				EmployeeID: lr.EmployeeID,
				PolicyID:   lr.PolicyID,
				StartDate:  lr.StartDate,
				EndDate:    lr.EndDate,
				IsUnpaid:   lr.IsUnpaid,
			}
			// Best-effort enrichment; IDs are always present.
			if emp, err := s.store.GetEmployee(ctx, orgID, lr.EmployeeID); err == nil {
				e.EmployeeName = strings.TrimSpace(emp.FirstName + " " + emp.LastName)
			}
			if pol, err := ls.GetTimeOffPolicy(ctx, orgID, lr.PolicyID); err == nil {
				e.PolicyName = pol.Name
			}
			entries = append(entries, e)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if entries == nil {
		entries = []whoIsOnLeaveEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"date": date, "on_leave": entries})
}
