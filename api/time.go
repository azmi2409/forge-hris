// Package api — component 2: time tracking routes.
//
// Thin translation over store.TimeTrackingStore: parse HTTP into store
// calls, map store errors to HTTP codes. RBAC summary:
//   - time:read scope for all reads; time:write for all writes.
//   - Members are self-scoped: they may only read/write their own entries,
//     hour entries, schedules, and shifts.
//   - Managers+ may act for any employee; pay-period definition/project/
//     schedule/shift management needs manager+; pay-period submit/approve/
//     reject needs manager+, reopen needs admin+.
//   - Clock in/out and break start/end REQUIRE an Idempotency-Key header
//     (double-punch protection on top of the state machine).
//
// NOTE (for the OpenAPI merge): mapError in server.go is shared, so the
// The component 2 sentinels are mapped by the shared mapError in
// server.go: ErrAlreadyClockedIn -> 409 ALREADY_CLOCKED_IN,
// ErrPeriodLocked -> 409 PERIOD_LOCKED, ErrInvalidTransition -> 409
// INVALID_TRANSITION.
package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// timeEmployeeScope resolves which employee the caller may act on. Members
// are restricted to themselves; managers and above may name any employee
// (defaulting to their own for PATs).
func timeEmployeeScope(w http.ResponseWriter, r *http.Request, ai *AuthInfo, requested string) (string, bool) {
	if effectiveRole(ai) == store.RoleMember {
		self := ""
		if ai.EmployeeID != nil {
			self = *ai.EmployeeID
		}
		if self == "" {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN", "no employee identity on credential")
			return "", false
		}
		if requested != "" && requested != self {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN",
				"members may only manage their own time records")
			return "", false
		}
		return self, true
	}
	if requested != "" {
		return requested, true
	}
	if ai.EmployeeID != nil && *ai.EmployeeID != "" {
		return *ai.EmployeeID, true
	}
	writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "employee_id: required")
	return "", false
}

// requireIdempotencyKey enforces the Idempotency-Key header on the
// double-punch-risk endpoints (clock in/out, break start/end).
func requireIdempotencyKey(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Idempotency-Key") == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"Idempotency-Key: required for clock operations")
		return false
	}
	return true
}

// parseDateParam parses a YYYY-MM-DD query/body date into midnight UTC.
func parseDateParam(w http.ResponseWriter, r *http.Request, v, field string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			field+": must be YYYY-MM-DD")
		return time.Time{}, false
	}
	return t.UTC(), true
}

// parseTimeParam parses an RFC3339 query timestamp.
func parseTimeParam(w http.ResponseWriter, r *http.Request, v, field string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			field+": must be RFC3339, e.g. 2026-10-16T09:00:00Z")
		return time.Time{}, false
	}
	return t.UTC(), true
}

func (s *Server) timeRoutes() {
	m := s.mux
	base := "/api/v1/organizations/{orgID}"

	m.HandleFunc("POST "+base+"/clock-in",
		s.withAudit("time_entry.clock_in", "time_entry", s.clockIn))
	m.HandleFunc("POST "+base+"/clock-out",
		s.withAudit("time_entry.clock_out", "time_entry", s.clockOut))
	m.HandleFunc("POST "+base+"/breaks/start",
		s.withAudit("time_entry.break_start", "time_entry", s.startBreak))
	m.HandleFunc("POST "+base+"/breaks/end",
		s.withAudit("time_entry.break_end", "time_entry", s.endBreak))

	m.HandleFunc("POST "+base+"/time-entries",
		s.withAudit("time_entry.created", "time_entry", s.createTimeEntry))
	m.HandleFunc("GET "+base+"/time-entries", s.handleListTimeEntries)
	m.HandleFunc("GET "+base+"/time-entries/{entryID}", s.handleGetTimeEntry)
	m.HandleFunc("PATCH "+base+"/time-entries/{entryID}",
		s.withAudit("time_entry.updated", "time_entry", s.patchTimeEntry))
	m.HandleFunc("POST "+base+"/time-entries/{entryID}/correct",
		s.withAudit("time_entry.corrected", "time_entry", s.correctTimeEntry))

	m.HandleFunc("POST "+base+"/hour-entries",
		s.withAudit("hour_entry.created", "hour_entry", s.createHourEntry))
	m.HandleFunc("GET "+base+"/hour-entries", s.handleListHourEntries)
	m.HandleFunc("GET "+base+"/hour-entries/{entryID}", s.handleGetHourEntry)
	m.HandleFunc("PATCH "+base+"/hour-entries/{entryID}",
		s.withAudit("hour_entry.updated", "hour_entry", s.patchHourEntry))
	m.HandleFunc("DELETE "+base+"/hour-entries/{entryID}",
		s.withAudit("hour_entry.deleted", "hour_entry", s.deleteHourEntry))

	m.HandleFunc("POST "+base+"/projects",
		s.withAudit("project.created", "project", s.createProject))
	m.HandleFunc("GET "+base+"/projects", s.handleListProjects)
	m.HandleFunc("GET "+base+"/projects/{projectID}", s.handleGetProject)
	m.HandleFunc("PATCH "+base+"/projects/{projectID}",
		s.withAudit("project.updated", "project", s.patchProject))
	m.HandleFunc("DELETE "+base+"/projects/{projectID}",
		s.withAudit("project.deleted", "project", s.deleteProject))

	m.HandleFunc("POST "+base+"/pay-period-definitions",
		s.withAudit("pay_period_definition.created", "pay_period_definition", s.createPayPeriodDefinition))
	m.HandleFunc("GET "+base+"/pay-period-definitions", s.handleListPayPeriodDefinitions)
	m.HandleFunc("GET "+base+"/pay-period-definitions/{definitionID}", s.handleGetPayPeriodDefinition)
	m.HandleFunc("PATCH "+base+"/pay-period-definitions/{definitionID}",
		s.withAudit("pay_period_definition.updated", "pay_period_definition", s.patchPayPeriodDefinition))
	m.HandleFunc("DELETE "+base+"/pay-period-definitions/{definitionID}",
		s.withAudit("pay_period_definition.deleted", "pay_period_definition", s.deletePayPeriodDefinition))

	m.HandleFunc("POST "+base+"/pay-periods",
		s.withAudit("pay_period.created", "pay_period", s.createPayPeriod))
	m.HandleFunc("GET "+base+"/pay-periods", s.handleListPayPeriods)
	m.HandleFunc("GET "+base+"/pay-periods/{periodID}", s.handleGetPayPeriod)
	m.HandleFunc("POST "+base+"/pay-periods/{periodID}/submit",
		s.withAudit("pay_period.submitted", "pay_period", s.submitPayPeriod))
	m.HandleFunc("POST "+base+"/pay-periods/{periodID}/approve",
		s.withAudit("pay_period.approved", "pay_period", s.approvePayPeriod))
	m.HandleFunc("POST "+base+"/pay-periods/{periodID}/reject",
		s.withAudit("pay_period.rejected", "pay_period", s.rejectPayPeriod))
	m.HandleFunc("POST "+base+"/pay-periods/{periodID}/reopen",
		s.withAudit("pay_period.reopened", "pay_period", s.reopenPayPeriod))

	m.HandleFunc("POST "+base+"/schedules",
		s.withAudit("schedule.created", "schedule", s.createSchedule))
	m.HandleFunc("GET "+base+"/schedules", s.handleListSchedules)
	m.HandleFunc("GET "+base+"/schedules/{scheduleID}", s.handleGetSchedule)
	m.HandleFunc("PATCH "+base+"/schedules/{scheduleID}",
		s.withAudit("schedule.updated", "schedule", s.patchSchedule))
	m.HandleFunc("DELETE "+base+"/schedules/{scheduleID}",
		s.withAudit("schedule.deleted", "schedule", s.deleteSchedule))

	m.HandleFunc("POST "+base+"/shifts",
		s.withAudit("shift.created", "shift", s.createShift))
	m.HandleFunc("GET "+base+"/shifts", s.handleListShifts)
	m.HandleFunc("GET "+base+"/shifts/{shiftID}", s.handleGetShift)
	m.HandleFunc("PATCH "+base+"/shifts/{shiftID}",
		s.withAudit("shift.updated", "shift", s.patchShift))
	m.HandleFunc("DELETE "+base+"/shifts/{shiftID}",
		s.withAudit("shift.deleted", "shift", s.deleteShift))
	m.HandleFunc("POST "+base+"/shifts/publish",
		s.withAudit("shift.published", "shift", s.publishShifts))

	m.HandleFunc("POST "+base+"/overtime/preview", s.previewOvertime)
}

// --- clock ---

type clockRequest struct {
	EmployeeID *string    `json:"employee_id,omitempty"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	ProjectID  *string    `json:"project_id,omitempty"`
	ActivityID *string    `json:"activity_id,omitempty"`
	LocationID *string    `json:"location_id,omitempty"`
	Latitude   *float64   `json:"latitude,omitempty"`
	Longitude  *float64   `json:"longitude,omitempty"`
	Address    *string    `json:"address,omitempty"`
	ClientType *string    `json:"client_type,omitempty"`
}

func (req clockRequest) toEntry() store.TimeEntry {
	e := store.TimeEntry{
		ProjectID:  derefOrEmpty(req.ProjectID),
		ActivityID: derefOrEmpty(req.ActivityID),
		LocationID: derefOrEmpty(req.LocationID),
		Latitude:   req.Latitude,
		Longitude:  req.Longitude,
		Address:    req.Address,
	}
	if req.OccurredAt != nil {
		e.OccurredAt = req.OccurredAt.UTC()
	}
	if req.ClientType != nil && strings.TrimSpace(*req.ClientType) != "" {
		e.ClientType = strings.TrimSpace(*req.ClientType)
	}
	return e
}

// clockOp is the shared body of the four clock endpoints.
func (s *Server) clockOp(w http.ResponseWriter, r *http.Request, ai *AuthInfo,
	op func(ctx context.Context, orgID, employeeID string, e store.TimeEntry) (store.TimeEntry, error),
) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeWrite) {
		return zero, false
	}
	if !requireIdempotencyKey(w, r) {
		return zero, false
	}
	var req clockRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	var requested string
	if req.EmployeeID != nil {
		requested = *req.EmployeeID
	}
	employeeID, ok := timeEmployeeScope(w, r, ai, requested)
	if !ok {
		return zero, false
	}
	entry, err := op(r.Context(), orgID, employeeID, req.toEntry())
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, entry)
	return auditData{ResourceID: entry.ID, After: entry}, true
}

func (s *Server) clockIn(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.clockOp(w, r, ai, s.store.ClockIn)
}

func (s *Server) clockOut(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.clockOp(w, r, ai, s.store.ClockOut)
}

func (s *Server) startBreak(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.clockOp(w, r, ai, s.store.StartBreak)
}

func (s *Server) endBreak(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.clockOp(w, r, ai, s.store.EndBreak)
}

// --- time entries ---

type createTimeEntryRequest struct {
	EmployeeID string     `json:"employee_id"`
	Kind       string     `json:"kind"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	ProjectID  *string    `json:"project_id,omitempty"`
	ActivityID *string    `json:"activity_id,omitempty"`
	LocationID *string    `json:"location_id,omitempty"`
	Latitude   *float64   `json:"latitude,omitempty"`
	Longitude  *float64   `json:"longitude,omitempty"`
	Address    *string    `json:"address,omitempty"`
	ClientType *string    `json:"client_type,omitempty"`
}

// createTimeEntry stores a manual/backfill punch without the clock state
// machine. Manager+ only; members punch through the clock endpoints.
func (s *Server) createTimeEntry(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to create manual time entries") {
		return zero, false
	}
	var req createTimeEntryRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	kind := store.TimeEntryKind(req.Kind)
	if !store.ValidTimeEntryKind(kind) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"kind must be one of in, out, start_break, end_break")
		return zero, false
	}
	if strings.TrimSpace(req.EmployeeID) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "employee_id: required")
		return zero, false
	}
	e := store.TimeEntry{
		OrganizationID: orgID,
		EmployeeID:     req.EmployeeID,
		Kind:           kind,
		ProjectID:      derefOrEmpty(req.ProjectID),
		ActivityID:     derefOrEmpty(req.ActivityID),
		LocationID:     derefOrEmpty(req.LocationID),
		Latitude:       req.Latitude,
		Longitude:      req.Longitude,
		Address:        req.Address,
	}
	if req.OccurredAt != nil {
		e.OccurredAt = req.OccurredAt.UTC()
	} else {
		e.OccurredAt = time.Now().UTC()
	}
	if req.ClientType != nil && strings.TrimSpace(*req.ClientType) != "" {
		e.ClientType = strings.TrimSpace(*req.ClientType)
	}
	created, err := s.store.CreateTimeEntry(r.Context(), e)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) timeEntryFilter(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (store.TimeEntryFilter, bool) {
	var f store.TimeEntryFilter
	q := r.URL.Query()
	if effectiveRole(ai) == store.RoleMember {
		if ai.EmployeeID == nil || *ai.EmployeeID == "" {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN", "no employee identity on credential")
			return f, false
		}
		f.EmployeeID = *ai.EmployeeID
	} else {
		f.EmployeeID = q.Get("employee_id")
	}
	if v := q.Get("kind"); v != "" {
		kind := store.TimeEntryKind(v)
		if !store.ValidTimeEntryKind(kind) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"kind must be one of in, out, start_break, end_break")
			return f, false
		}
		f.Kind = kind
	}
	if v := q.Get("from"); v != "" {
		t, ok := parseTimeParam(w, r, v, "from")
		if !ok {
			return f, false
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, ok := parseTimeParam(w, r, v, "to")
		if !ok {
			return f, false
		}
		f.To = &t
	}
	f.IncludeDeleted = q.Get("include_deleted") == "true"
	f.IncludeHistory = q.Get("include_history") == "true"
	return f, true
}

func (s *Server) handleListTimeEntries(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	filter, ok := s.timeEntryFilter(w, r, ai)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	entries, next, err := s.store.ListTimeEntries(r.Context(), orgID, filter, page)
	if !mapError(w, r, err) {
		return
	}
	if entries == nil {
		entries = []store.TimeEntry{}
	}
	writePage(w, entries, next)
}

func (s *Server) handleGetTimeEntry(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	entryID := r.PathValue("entryID")
	entry, err := s.store.GetTimeEntry(r.Context(), orgID, entryID)
	if !mapError(w, r, err) {
		return
	}
	if _, ok := timeEmployeeScope(w, r, ai, entry.EmployeeID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

type patchTimeEntryRequest struct {
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	ProjectID  *string    `json:"project_id,omitempty"`
	ActivityID *string    `json:"activity_id,omitempty"`
	LocationID *string    `json:"location_id,omitempty"`
	Latitude   *float64   `json:"latitude,omitempty"`
	Longitude  *float64   `json:"longitude,omitempty"`
	Address    *string    `json:"address,omitempty"`
	ClientType *string    `json:"client_type,omitempty"`
	Version    int64      `json:"version"`
}

// patchTimeEntry applies a sparse update to a mutable entry (not locked,
// not inside a frozen period). Kind changes go through /correct.
func (s *Server) patchTimeEntry(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeWrite) {
		return zero, false
	}
	var req patchTimeEntryRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	entryID := r.PathValue("entryID")
	cur, err := s.store.GetTimeEntry(r.Context(), orgID, entryID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if _, ok := timeEmployeeScope(w, r, ai, cur.EmployeeID); !ok {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.OccurredAt != nil {
		cur.OccurredAt = req.OccurredAt.UTC()
	}
	if req.ProjectID != nil {
		cur.ProjectID = derefOrEmpty(req.ProjectID)
	}
	if req.ActivityID != nil {
		cur.ActivityID = derefOrEmpty(req.ActivityID)
	}
	if req.LocationID != nil {
		cur.LocationID = derefOrEmpty(req.LocationID)
	}
	if req.Latitude != nil {
		cur.Latitude = req.Latitude
	}
	if req.Longitude != nil {
		cur.Longitude = req.Longitude
	}
	if req.Address != nil {
		cur.Address = req.Address
	}
	if req.ClientType != nil {
		cur.ClientType = strings.TrimSpace(*req.ClientType)
	}
	updated, err := s.store.UpdateTimeEntry(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

type correctTimeEntryRequest struct {
	Kind       string     `json:"kind"`
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
	ProjectID  *string    `json:"project_id,omitempty"`
	ActivityID *string    `json:"activity_id,omitempty"`
	LocationID *string    `json:"location_id,omitempty"`
	Latitude   *float64   `json:"latitude,omitempty"`
	Longitude  *float64   `json:"longitude,omitempty"`
	Address    *string    `json:"address,omitempty"`
	ClientType *string    `json:"client_type,omitempty"`
}

// correctTimeEntry creates a superseding entry linked via prev/next. The
// original is never edited: it is frozen and linked forward. Corrections,
// never edits (Jibble/REA pattern).
func (s *Server) correctTimeEntry(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeWrite) {
		return zero, false
	}
	var req correctTimeEntryRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	kind := store.TimeEntryKind(req.Kind)
	if !store.ValidTimeEntryKind(kind) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"kind must be one of in, out, start_break, end_break")
		return zero, false
	}
	entryID := r.PathValue("entryID")
	orig, err := s.store.GetTimeEntry(r.Context(), orgID, entryID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if _, ok := timeEmployeeScope(w, r, ai, orig.EmployeeID); !ok {
		return zero, false
	}
	correction := store.TimeEntry{
		Kind:       kind,
		ProjectID:  derefOrEmpty(req.ProjectID),
		ActivityID: derefOrEmpty(req.ActivityID),
		LocationID: derefOrEmpty(req.LocationID),
		Latitude:   req.Latitude,
		Longitude:  req.Longitude,
		Address:    req.Address,
	}
	if req.OccurredAt != nil {
		correction.OccurredAt = req.OccurredAt.UTC()
	}
	if req.ClientType != nil && strings.TrimSpace(*req.ClientType) != "" {
		correction.ClientType = strings.TrimSpace(*req.ClientType)
	}
	created, err := s.store.CorrectTimeEntry(r.Context(), orgID, entryID, correction)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, Before: orig, After: created}, true
}

// --- hour entries ---

type hourEntryRequest struct {
	EmployeeID *string `json:"employee_id,omitempty"`
	WorkDate   string  `json:"work_date"` // YYYY-MM-DD
	Hours      float64 `json:"hours"`
	ProjectID  *string `json:"project_id,omitempty"`
	Note       *string `json:"note,omitempty"`
	Version    int64   `json:"version,omitempty"` // PATCH only
}

func (s *Server) createHourEntry(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeWrite) {
		return zero, false
	}
	var req hourEntryRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	workDate, ok := parseDateParam(w, r, req.WorkDate, "work_date")
	if !ok {
		return zero, false
	}
	var requested string
	if req.EmployeeID != nil {
		requested = *req.EmployeeID
	}
	employeeID, ok := timeEmployeeScope(w, r, ai, requested)
	if !ok {
		return zero, false
	}
	e := store.HourEntry{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		WorkDate:       workDate,
		Hours:          req.Hours,
		ProjectID:      derefOrEmpty(req.ProjectID),
	}
	if req.Note != nil {
		e.Note = *req.Note
	}
	created, err := s.store.CreateHourEntry(r.Context(), e)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListHourEntries(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	var f store.HourEntryFilter
	q := r.URL.Query()
	if effectiveRole(ai) == store.RoleMember {
		if ai.EmployeeID == nil || *ai.EmployeeID == "" {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN", "no employee identity on credential")
			return
		}
		f.EmployeeID = *ai.EmployeeID
	} else {
		f.EmployeeID = q.Get("employee_id")
	}
	if v := q.Get("from"); v != "" {
		t, ok := parseDateParam(w, r, v, "from")
		if !ok {
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, ok := parseDateParam(w, r, v, "to")
		if !ok {
			return
		}
		f.To = &t
	}
	f.IncludeDeleted = q.Get("include_deleted") == "true"
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	entries, next, err := s.store.ListHourEntries(r.Context(), orgID, f, page)
	if !mapError(w, r, err) {
		return
	}
	if entries == nil {
		entries = []store.HourEntry{}
	}
	writePage(w, entries, next)
}

func (s *Server) handleGetHourEntry(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	entry, err := s.store.GetHourEntry(r.Context(), orgID, r.PathValue("entryID"))
	if !mapError(w, r, err) {
		return
	}
	if _, ok := timeEmployeeScope(w, r, ai, entry.EmployeeID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) patchHourEntry(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeWrite) {
		return zero, false
	}
	var req hourEntryRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	entryID := r.PathValue("entryID")
	cur, err := s.store.GetHourEntry(r.Context(), orgID, entryID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if _, ok := timeEmployeeScope(w, r, ai, cur.EmployeeID); !ok {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.WorkDate != "" {
		workDate, ok := parseDateParam(w, r, req.WorkDate, "work_date")
		if !ok {
			return zero, false
		}
		cur.WorkDate = workDate
	}
	if req.Hours != 0 {
		cur.Hours = req.Hours
	}
	if req.ProjectID != nil {
		cur.ProjectID = derefOrEmpty(req.ProjectID)
	}
	if req.Note != nil {
		cur.Note = *req.Note
	}
	updated, err := s.store.UpdateHourEntry(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

type versionRequest struct {
	Version int64 `json:"version"`
}

func (s *Server) deleteHourEntry(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeWrite) {
		return zero, false
	}
	var req versionRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	entryID := r.PathValue("entryID")
	before, err := s.store.GetHourEntry(r.Context(), orgID, entryID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if _, ok := timeEmployeeScope(w, r, ai, before.EmployeeID); !ok {
		return zero, false
	}
	if _, err := s.store.DeleteHourEntry(r.Context(), orgID, entryID, req.Version); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: entryID, Before: before}, true
}

// --- projects ---

type projectRequest struct {
	Name       string  `json:"name"`
	ClientName *string `json:"client_name,omitempty"`
	Version    int64   `json:"version,omitempty"` // PATCH only
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to create projects") {
		return zero, false
	}
	var req projectRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "name: required")
		return zero, false
	}
	p := store.Project{OrganizationID: orgID, Name: req.Name}
	if req.ClientName != nil {
		p.ClientName = *req.ClientName
	}
	created, err := s.store.CreateProject(r.Context(), p)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	projects, next, err := s.store.ListProjects(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	if projects == nil {
		projects = []store.Project{}
	}
	writePage(w, projects, next)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	p, err := s.store.GetProject(r.Context(), orgID, r.PathValue("projectID"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) patchProject(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to update projects") {
		return zero, false
	}
	var req projectRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := s.store.GetProject(r.Context(), orgID, r.PathValue("projectID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if strings.TrimSpace(req.Name) != "" {
		cur.Name = req.Name
	}
	if req.ClientName != nil {
		cur.ClientName = *req.ClientName
	}
	updated, err := s.store.UpdateProject(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to delete projects") {
		return zero, false
	}
	projectID := r.PathValue("projectID")
	before, err := s.store.GetProject(r.Context(), orgID, projectID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if err := s.store.DeleteProject(r.Context(), orgID, projectID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: projectID, Before: before}, true
}

// --- pay-period definitions ---

type payPeriodDefinitionRequest struct {
	Name      string `json:"name"`
	Cadence   string `json:"cadence"`
	AnchorDay *int   `json:"anchor_day,omitempty"`
	Version   int64  `json:"version,omitempty"` // PATCH only
}

func (s *Server) createPayPeriodDefinition(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to create pay period definitions") {
		return zero, false
	}
	var req payPeriodDefinitionRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	cadence := store.PayPeriodCadence(req.Cadence)
	if !store.ValidPayPeriodCadence(cadence) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"cadence must be one of Monthly, TwiceInMonth, Weekly")
		return zero, false
	}
	d := store.PayPeriodDefinition{
		OrganizationID: orgID,
		Name:           req.Name,
		Cadence:        cadence,
		AnchorDay:      1,
	}
	if req.AnchorDay != nil {
		d.AnchorDay = *req.AnchorDay
	}
	created, err := s.store.CreatePayPeriodDefinition(r.Context(), d)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListPayPeriodDefinitions(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	defs, next, err := s.store.ListPayPeriodDefinitions(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	if defs == nil {
		defs = []store.PayPeriodDefinition{}
	}
	writePage(w, defs, next)
}

func (s *Server) handleGetPayPeriodDefinition(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	d, err := s.store.GetPayPeriodDefinition(r.Context(), orgID, r.PathValue("definitionID"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) patchPayPeriodDefinition(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to update pay period definitions") {
		return zero, false
	}
	var req payPeriodDefinitionRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := s.store.GetPayPeriodDefinition(r.Context(), orgID, r.PathValue("definitionID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if strings.TrimSpace(req.Name) != "" {
		cur.Name = req.Name
	}
	if req.Cadence != "" {
		cadence := store.PayPeriodCadence(req.Cadence)
		if !store.ValidPayPeriodCadence(cadence) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"cadence must be one of Monthly, TwiceInMonth, Weekly")
			return zero, false
		}
		cur.Cadence = cadence
	}
	if req.AnchorDay != nil {
		cur.AnchorDay = *req.AnchorDay
	}
	updated, err := s.store.UpdatePayPeriodDefinition(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) deletePayPeriodDefinition(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to delete pay period definitions") {
		return zero, false
	}
	definitionID := r.PathValue("definitionID")
	before, err := s.store.GetPayPeriodDefinition(r.Context(), orgID, definitionID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if err := s.store.DeletePayPeriodDefinition(r.Context(), orgID, definitionID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: definitionID, Before: before}, true
}

// --- pay periods ---

type createPayPeriodRequest struct {
	DefinitionID string     `json:"definition_id"`
	PeriodStart  *time.Time `json:"period_start,omitempty"` // defaults to now
}

func (s *Server) createPayPeriod(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to create pay periods") {
		return zero, false
	}
	var req createPayPeriodRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if strings.TrimSpace(req.DefinitionID) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "definition_id: required")
		return zero, false
	}
	start := time.Now().UTC()
	if req.PeriodStart != nil {
		start = req.PeriodStart.UTC()
	}
	created, err := s.store.CreatePayPeriod(r.Context(), orgID, req.DefinitionID, start)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListPayPeriods(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	var f store.PayPeriodFilter
	q := r.URL.Query()
	f.DefinitionID = q.Get("definition_id")
	if v := q.Get("status"); v != "" {
		status := store.PayPeriodStatus(v)
		if !store.ValidPayPeriodStatus(status) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"status must be one of Open, Submitted, Approved, Rejected, Reopened")
			return
		}
		f.Status = status
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	periods, next, err := s.store.ListPayPeriods(r.Context(), orgID, f, page)
	if !mapError(w, r, err) {
		return
	}
	if periods == nil {
		periods = []store.PayPeriod{}
	}
	writePage(w, periods, next)
}

func (s *Server) handleGetPayPeriod(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	p, err := s.store.GetPayPeriod(r.Context(), orgID, r.PathValue("periodID"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// payPeriodTransition is the shared body of the four transition endpoints.
func (s *Server) payPeriodTransition(w http.ResponseWriter, r *http.Request, ai *AuthInfo,
	minRole store.Role,
	op func(ctx context.Context, orgID, id string, version int64) (store.PayPeriod, error),
) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, minRole, " to change pay period status") {
		return zero, false
	}
	var req versionRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	periodID := r.PathValue("periodID")
	before, err := s.store.GetPayPeriod(r.Context(), orgID, periodID)
	if !mapError(w, r, err) {
		return zero, false
	}
	updated, err := op(r.Context(), orgID, periodID, req.Version)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) submitPayPeriod(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.payPeriodTransition(w, r, ai, store.RoleManager, s.store.SubmitPayPeriod)
}

func (s *Server) approvePayPeriod(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.payPeriodTransition(w, r, ai, store.RoleManager, s.store.ApprovePayPeriod)
}

func (s *Server) rejectPayPeriod(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.payPeriodTransition(w, r, ai, store.RoleManager, s.store.RejectPayPeriod)
}

func (s *Server) reopenPayPeriod(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	return s.payPeriodTransition(w, r, ai, store.RoleAdmin, s.store.ReopenPayPeriod)
}

// --- schedules ---

type scheduleRequest struct {
	EmployeeID         *string  `json:"employee_id,omitempty"`
	Kind               string   `json:"kind"`
	ExpectedDailyHours *float64 `json:"expected_daily_hours,omitempty"`
	Version            int64    `json:"version,omitempty"` // PATCH only
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to create schedules") {
		return zero, false
	}
	var req scheduleRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	kind := store.ScheduleKind(req.Kind)
	if !store.ValidScheduleKind(kind) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"kind must be one of Fixed, Flexible, Weekly")
		return zero, false
	}
	var requested string
	if req.EmployeeID != nil {
		requested = *req.EmployeeID
	}
	employeeID, ok := timeEmployeeScope(w, r, ai, requested)
	if !ok {
		return zero, false
	}
	sc := store.Schedule{
		OrganizationID:     orgID,
		EmployeeID:         employeeID,
		Kind:               kind,
		ExpectedDailyHours: 8,
	}
	if req.ExpectedDailyHours != nil {
		sc.ExpectedDailyHours = *req.ExpectedDailyHours
	}
	created, err := s.store.CreateSchedule(r.Context(), sc)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	employeeID := r.URL.Query().Get("employee_id")
	// Members see only their own schedules; managers may filter or list all.
	var filter string
	if effectiveRole(ai) == store.RoleMember {
		if ai.EmployeeID == nil || *ai.EmployeeID == "" {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN", "no employee identity on credential")
			return
		}
		filter = *ai.EmployeeID
	} else {
		filter = employeeID
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	scheds, next, err := s.store.ListSchedules(r.Context(), orgID, filter, page)
	if !mapError(w, r, err) {
		return
	}
	if scheds == nil {
		scheds = []store.Schedule{}
	}
	writePage(w, scheds, next)
}

func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	sc, err := s.store.GetSchedule(r.Context(), orgID, r.PathValue("scheduleID"))
	if !mapError(w, r, err) {
		return
	}
	if _, ok := timeEmployeeScope(w, r, ai, sc.EmployeeID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) patchSchedule(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to update schedules") {
		return zero, false
	}
	var req scheduleRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := s.store.GetSchedule(r.Context(), orgID, r.PathValue("scheduleID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.Kind != "" {
		kind := store.ScheduleKind(req.Kind)
		if !store.ValidScheduleKind(kind) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"kind must be one of Fixed, Flexible, Weekly")
			return zero, false
		}
		cur.Kind = kind
	}
	if req.EmployeeID != nil && *req.EmployeeID != "" {
		cur.EmployeeID = *req.EmployeeID
	}
	if req.ExpectedDailyHours != nil {
		cur.ExpectedDailyHours = *req.ExpectedDailyHours
	}
	updated, err := s.store.UpdateSchedule(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to delete schedules") {
		return zero, false
	}
	scheduleID := r.PathValue("scheduleID")
	before, err := s.store.GetSchedule(r.Context(), orgID, scheduleID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if err := s.store.DeleteSchedule(r.Context(), orgID, scheduleID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: scheduleID, Before: before}, true
}

// --- shifts ---

type shiftRequest struct {
	EmployeeID *string    `json:"employee_id,omitempty"`
	ShiftDate  string     `json:"shift_date"` // YYYY-MM-DD
	StartsAt   *time.Time `json:"starts_at"`
	EndsAt     *time.Time `json:"ends_at"`
	Published  *bool      `json:"published,omitempty"`
	Version    int64      `json:"version,omitempty"` // PATCH only
}

func (s *Server) createShift(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to create shifts") {
		return zero, false
	}
	var req shiftRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	shiftDate, ok := parseDateParam(w, r, req.ShiftDate, "shift_date")
	if !ok {
		return zero, false
	}
	if req.StartsAt == nil || req.EndsAt == nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "starts_at and ends_at are required")
		return zero, false
	}
	var requested string
	if req.EmployeeID != nil {
		requested = *req.EmployeeID
	}
	employeeID, ok := timeEmployeeScope(w, r, ai, requested)
	if !ok {
		return zero, false
	}
	sh := store.Shift{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		ShiftDate:      shiftDate,
		StartsAt:       req.StartsAt.UTC(),
		EndsAt:         req.EndsAt.UTC(),
	}
	if req.Published != nil {
		sh.Published = *req.Published
	}
	created, err := s.store.CreateShift(r.Context(), sh)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListShifts(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	var f store.ShiftFilter
	q := r.URL.Query()
	// Members see only their own shifts; managers may filter or see all.
	if effectiveRole(ai) == store.RoleMember {
		if ai.EmployeeID == nil || *ai.EmployeeID == "" {
			writeError(w, r, http.StatusForbidden, "FORBIDDEN", "no employee identity on credential")
			return
		}
		f.EmployeeID = *ai.EmployeeID
	} else {
		f.EmployeeID = q.Get("employee_id")
	}
	if v := q.Get("from"); v != "" {
		t, ok := parseDateParam(w, r, v, "from")
		if !ok {
			return
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, ok := parseDateParam(w, r, v, "to")
		if !ok {
			return
		}
		f.To = &t
	}
	if v := q.Get("published"); v != "" {
		p := v == "true"
		if v != "true" && v != "false" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "published must be true or false")
			return
		}
		f.Published = &p
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	shifts, next, err := s.store.ListShifts(r.Context(), orgID, f, page)
	if !mapError(w, r, err) {
		return
	}
	if shifts == nil {
		shifts = []store.Shift{}
	}
	writePage(w, shifts, next)
}

func (s *Server) handleGetShift(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	sh, err := s.store.GetShift(r.Context(), orgID, r.PathValue("shiftID"))
	if !mapError(w, r, err) {
		return
	}
	if _, ok := timeEmployeeScope(w, r, ai, sh.EmployeeID); !ok {
		return
	}
	writeJSON(w, http.StatusOK, sh)
}

func (s *Server) patchShift(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to update shifts") {
		return zero, false
	}
	var req shiftRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := s.store.GetShift(r.Context(), orgID, r.PathValue("shiftID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.ShiftDate != "" {
		shiftDate, ok := parseDateParam(w, r, req.ShiftDate, "shift_date")
		if !ok {
			return zero, false
		}
		cur.ShiftDate = shiftDate
	}
	if req.StartsAt != nil {
		cur.StartsAt = req.StartsAt.UTC()
	}
	if req.EndsAt != nil {
		cur.EndsAt = req.EndsAt.UTC()
	}
	if req.EmployeeID != nil && *req.EmployeeID != "" {
		cur.EmployeeID = *req.EmployeeID
	}
	if req.Published != nil {
		cur.Published = *req.Published
	}
	updated, err := s.store.UpdateShift(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) deleteShift(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to delete shifts") {
		return zero, false
	}
	shiftID := r.PathValue("shiftID")
	before, err := s.store.GetShift(r.Context(), orgID, shiftID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if err := s.store.DeleteShift(r.Context(), orgID, shiftID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: shiftID, Before: before}, true
}

type publishShiftsRequest struct {
	IDs []string `json:"ids"`
}

// publishShifts bulk-publishes draft shifts so employees can see them.
// Idempotent: already-published shifts are skipped and not counted.
func (s *Server) publishShifts(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to publish shifts") {
		return zero, false
	}
	var req publishShiftsRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if len(req.IDs) == 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "ids: at least one shift id is required")
		return zero, false
	}
	n, err := s.store.PublishShifts(r.Context(), orgID, req.IDs)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, map[string]any{"published": n})
	return auditData{ResourceID: strings.Join(req.IDs, ","), After: map[string]any{"published": n}}, true
}

// --- overtime preview ---

type overtimePreviewRequest struct {
	EmployeeID *string             `json:"employee_id,omitempty"`
	From       string              `json:"from"` // YYYY-MM-DD
	To         string              `json:"to"`   // YYYY-MM-DD, exclusive
	Rules      store.OvertimeRules `json:"rules"`
	Holidays   []string            `json:"holidays,omitempty"` // YYYY-MM-DD
}

// previewOvertime is the read-only counterpart of the payroll-bound overtime
// engine: list the employee's entries over [from, to) and run the pure
// ComputeOvertime over them. No state changes, so no audit event and no
// idempotency key.
func (s *Server) previewOvertime(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeTimeRead) {
		return
	}
	var req overtimePreviewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	from, ok := parseDateParam(w, r, req.From, "from")
	if !ok {
		return
	}
	to, ok := parseDateParam(w, r, req.To, "to")
	if !ok {
		return
	}
	if !to.After(from) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "to must be after from")
		return
	}
	var requested string
	if req.EmployeeID != nil {
		requested = *req.EmployeeID
	}
	employeeID, ok := timeEmployeeScope(w, r, ai, requested)
	if !ok {
		return
	}
	holidays := map[string]bool{}
	for _, h := range req.Holidays {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"holidays must be YYYY-MM-DD")
			return
		}
		holidays[h] = true
	}
	// Walk the full range (entries are small; the store caps pages).
	var entries []store.TimeEntry
	page := store.Page{Limit: store.MaxListLimit}
	for {
		batch, next, err := s.store.ListTimeEntries(r.Context(), orgID, store.TimeEntryFilter{
			EmployeeID: employeeID,
			From:       &from,
			To:         &to,
		}, page)
		if !mapError(w, r, err) {
			return
		}
		entries = append(entries, batch...)
		if next == "" {
			break
		}
		page.Cursor = next
	}
	result := store.ComputeOvertime(entries, req.Rules, holidays)
	writeJSON(w, http.StatusOK, map[string]any{
		"employee_id": employeeID,
		"from":        req.From,
		"to":          req.To,
		"entries":     len(entries),
		"result":      result,
	})
}
