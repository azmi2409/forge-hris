package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

// --- argument helpers ---

func toolErr(code, msg string) *ToolError {
	return &ToolError{Code: code, Message: msg}
}

func validationErr(field, msg string) *ToolError {
	return &ToolError{Code: "VALIDATION_ERROR", Message: msg, Details: map[string]any{"field": field}}
}

// mapStoreError converts store sentinel errors to the same UPPER_SNAKE
// codes the REST mapError uses, so both surfaces speak one error language.
func mapStoreError(err error) *ToolError {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return toolErr("NOT_FOUND", err.Error())
	case errors.Is(err, store.ErrAlreadyExists):
		return toolErr("ALREADY_EXISTS", err.Error())
	case errors.Is(err, store.ErrVersionConflict):
		return toolErr("VERSION_CONFLICT", "resource changed since it was read; fetch it again and retry")
	case errors.Is(err, store.ErrNotEmpty):
		return toolErr("GROUP_NOT_EMPTY", err.Error())
	case errors.Is(err, store.ErrAlreadyClockedIn):
		return toolErr("ALREADY_CLOCKED_IN", "employee already has an open time entry")
	case errors.Is(err, store.ErrPeriodLocked):
		return toolErr("PERIOD_LOCKED", "entry falls in a submitted or approved pay period and cannot be changed")
	case errors.Is(err, store.ErrInvalidTransition):
		return toolErr("INVALID_TRANSITION", "state transition not allowed: "+err.Error())
	case errors.Is(err, store.ErrInsufficientBalance):
		return toolErr("INSUFFICIENT_BALANCE", err.Error())
	case errors.Is(err, store.ErrNotCurrentApprover):
		return toolErr("NOT_CURRENT_APPROVER", err.Error())
	}
	var berr *store.BulkApprovalError
	if errors.As(err, &berr) {
		return &ToolError{Code: "BULK_APPROVAL_FAILED", Message: "bulk approval failed; nothing was applied",
			Details: map[string]any{"results": berr.Results}}
	}
	var ve *store.ValidationError
	if errors.As(err, &ve) {
		return validationErr(ve.Field, ve.Message)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return toolErr("CANCELLED", "request cancelled")
	}
	return toolErr("INTERNAL", "internal error")
}

func reqString(args map[string]any, key string) (string, *ToolError) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", validationErr(key, "required")
	}
	s, ok := v.(string)
	if !ok || strings.TrimSpace(s) == "" {
		return "", validationErr(key, "required")
	}
	return strings.TrimSpace(s), nil
}

func optString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// strPtr mirrors the REST derefOrEmpty semantics: absent means "leave
// alone"; present-but-empty means "clear" (nil); otherwise the value.
func strPtr(args map[string]any, key string) (*string, bool) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false
	}
	s, ok := v.(string)
	if !ok {
		return nil, false
	}
	if strings.TrimSpace(s) == "" {
		return nil, true
	}
	out := strings.TrimSpace(s)
	return &out, true
}

func optFloat(args map[string]any, key string) (float64, *ToolError) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, validationErr(key, "must be a number")
	}
	return f, nil
}

func floatPtr(args map[string]any, key string) (*float64, *ToolError) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	f, ok := v.(float64)
	if !ok {
		return nil, validationErr(key, "must be a number")
	}
	return &f, nil
}

func optBool(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

func optStringSlice(args map[string]any, key string) ([]string, *ToolError) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	// Accept both []any (from JSON decoding) and []string (from Go callers
	// and tests).
	if ss, ok := v.([]string); ok {
		out := make([]string, 0, len(ss))
		for _, s := range ss {
			if strings.TrimSpace(s) == "" {
				return nil, validationErr(key, "must be an array of strings")
			}
			out = append(out, strings.TrimSpace(s))
		}
		return out, nil
	}
	raw, ok := v.([]any)
	if !ok {
		return nil, validationErr(key, "must be an array of strings")
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, validationErr(key, "must be an array of strings")
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}

// reqVersion reads the optimistic-concurrency token shared by every
// version-checked tool.
func reqVersion(args map[string]any) (int64, *ToolError) {
	v, ok := args["version"]
	if !ok || v == nil {
		return 0, validationErr("version", "required and must be positive")
	}
	f, ok := v.(float64)
	if !ok || f <= 0 || f != float64(int64(f)) {
		return 0, validationErr("version", "required and must be a positive integer")
	}
	return int64(f), nil
}

// parsePageArgs reads limit/cursor, mirroring the REST parsePage bounds.
func parsePageArgs(args map[string]any) (store.Page, *ToolError) {
	p := store.Page{Limit: 100, Cursor: optString(args, "cursor")}
	if v, ok := args["limit"]; ok && v != nil {
		f, ok := v.(float64)
		if !ok || f <= 0 || f != float64(int64(f)) {
			return p, validationErr("limit", "must be a positive integer")
		}
		n := int(f)
		if n > 1000 {
			n = 1000
		}
		p.Limit = n
	}
	return p, nil
}

// parseDateArg validates a YYYY-MM-DD argument.
func parseDateArg(args map[string]any, key string, required bool) (string, *ToolError) {
	s := optString(args, key)
	if s == "" {
		if required {
			return "", validationErr(key, "required (YYYY-MM-DD)")
		}
		return "", nil
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return "", validationErr(key, "must be YYYY-MM-DD")
	}
	return s, nil
}

// parseTimeArg parses an optional RFC3339 argument.
func parseTimeArg(args map[string]any, key string) (time.Time, bool, *ToolError) {
	s := optString(args, key)
	if s == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false, validationErr(key, "must be RFC3339, e.g. 2026-10-16T09:00:00Z")
	}
	return t.UTC(), true, nil
}

// pageEnvelope mirrors the REST writePage shape.
func pageEnvelope(data any, next string) map[string]any {
	if data == nil {
		data = []any{}
	}
	return map[string]any{"data": data, "next_cursor": next, "has_more": next != ""}
}

// --- employee scoping (mirrors api/time.go and api/leave.go) ---

// timeEmployee resolves which employee the caller may act on for time
// operations. Members are restricted to themselves; managers and above may
// name any employee (defaulting to their own for PATs).
func timeEmployee(ai *AuthInfo, requested string) (string, *ToolError) {
	if effectiveRole(ai) == store.RoleMember {
		self := ""
		if ai.EmployeeID != nil {
			self = *ai.EmployeeID
		}
		if self == "" {
			return "", toolErr("FORBIDDEN", "no employee identity on credential")
		}
		if requested != "" && requested != self {
			return "", toolErr("FORBIDDEN", "members may only manage their own time records")
		}
		return self, nil
	}
	if requested != "" {
		return requested, nil
	}
	if ai.EmployeeID != nil && *ai.EmployeeID != "" {
		return *ai.EmployeeID, nil
	}
	return "", validationErr("employee_id", "required")
}

// leaveSelfOrManager allows the employee themselves, or anyone with the
// manager role or higher.
func leaveSelfOrManager(ai *AuthInfo, employeeID, what string) *ToolError {
	if ai.EmployeeID != nil && *ai.EmployeeID == employeeID {
		return nil
	}
	if roleAtLeast(store.RoleManager, ai) {
		return nil
	}
	return toolErr("FORBIDDEN", "requires manager role or higher"+what)
}

// --- identity tools ---

func (s *Server) handleListEmployees(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	f := store.EmployeeFilter{
		GroupID: optString(args, "group_id"),
		Search:  optString(args, "q"),
	}
	if st := optString(args, "status"); st != "" {
		f.Status = store.InvitationStatus(st)
		if !store.ValidInvitationStatus(f.Status) {
			return nil, nil, validationErr("status", "must be one of not_invited, pending_invitation, joined, removed")
		}
	}
	p, terr := parsePageArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	emps, next, err := s.store.ListEmployees(ctx, ai.OrgID, f, p)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if emps == nil {
		emps = []store.Employee{}
	}
	return pageEnvelope(emps, next),
		&auditRecord{action: "employee.listed", resourceType: "employee"}, nil
}

func (s *Server) handleGetEmployee(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	employeeID, terr := reqString(args, "employee_id")
	if terr != nil {
		return nil, nil, terr
	}
	// Members may only read their own record (mirrors handleGetEmployee).
	if effectiveRole(ai) == store.RoleMember &&
		(ai.EmployeeID == nil || *ai.EmployeeID != employeeID) {
		return nil, nil, toolErr("FORBIDDEN", "members may only read their own employee record")
	}
	emp, err := s.store.GetEmployee(ctx, ai.OrgID, employeeID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"employee": emp},
		&auditRecord{action: "employee.read", resourceType: "employee", resourceID: emp.ID}, nil
}

// employeeNumberOrGenerate mirrors the REST helper: use the requested
// number, or mint EMP-<8 hex> when absent.
func employeeNumberOrGenerate(req *string) (string, *ToolError) {
	if req != nil && strings.TrimSpace(*req) != "" {
		return strings.TrimSpace(*req), nil
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", toolErr("INTERNAL", "internal error")
	}
	return "EMP-" + hex.EncodeToString(b[:]), nil
}

func (s *Server) handleCreateEmployee(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	firstName, terr := reqString(args, "first_name")
	if terr != nil {
		return nil, nil, terr
	}
	lastName, terr := reqString(args, "last_name")
	if terr != nil {
		return nil, nil, terr
	}
	email, terr := reqString(args, "email")
	if terr != nil {
		return nil, nil, terr
	}
	if !strings.Contains(email, "@") {
		return nil, nil, validationErr("email", "must be a valid email address")
	}
	var numberReq *string
	if p, present := strPtr(args, "employee_number"); present {
		numberReq = p
	}
	number, terr := employeeNumberOrGenerate(numberReq)
	if terr != nil {
		return nil, nil, terr
	}
	emp := store.Employee{
		OrganizationID: ai.OrgID,
		EmployeeNumber: number,
		FirstName:      firstName,
		LastName:       lastName,
		Email:          email,
	}
	if p, present := strPtr(args, "position"); present && p != nil {
		emp.Position = *p
	}
	if p, present := strPtr(args, "employment_type"); present {
		if p == nil || *p == "" {
			return nil, nil, validationErr("employment_type", "must be one of full_time, part_time, contract, intern")
		}
		et := store.EmploymentType(*p)
		if !store.ValidEmploymentType(et) {
			return nil, nil, validationErr("employment_type", "must be one of full_time, part_time, contract, intern")
		}
		emp.EmploymentType = et
	}
	if p, present := strPtr(args, "group_id"); present {
		emp.GroupID = p
	}
	if p, present := strPtr(args, "manager_id"); present {
		emp.ManagerID = p
	}
	if p, present := strPtr(args, "time_zone"); present && p != nil {
		emp.TimeZone = *p
	}
	if ds := optString(args, "work_start_date"); ds != "" {
		t, err := time.Parse("2006-01-02", ds)
		if err != nil {
			return nil, nil, validationErr("work_start_date", "must be YYYY-MM-DD")
		}
		utc := t.UTC()
		emp.WorkStartDate = &utc
	}
	created, err := s.store.CreateEmployee(ctx, emp)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	// New hires start as members once joined (mirrors createEmployee).
	if created.Status == store.InvitationJoined {
		grantedBy := ai.CredentialID
		if ai.EmployeeID != nil && *ai.EmployeeID != "" {
			grantedBy = *ai.EmployeeID
		}
		if err := s.store.SetRole(ctx, ai.OrgID, created.ID, store.RoleMember, grantedBy); err != nil {
			return nil, nil, mapStoreError(err)
		}
	}
	return map[string]any{"employee": created},
		&auditRecord{action: "employee.created", resourceType: "employee", resourceID: created.ID, after: created}, nil
}

func (s *Server) handleUpdateEmployee(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	employeeID, terr := reqString(args, "employee_id")
	if terr != nil {
		return nil, nil, terr
	}
	version, terr := reqVersion(args)
	if terr != nil {
		return nil, nil, terr
	}
	if v, ok := args["email"]; ok && v != nil {
		if es, ok := v.(string); ok && es != "" && !strings.Contains(es, "@") {
			return nil, nil, validationErr("email", "invalid")
		}
	}
	cur, err := s.store.GetEmployee(ctx, ai.OrgID, employeeID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	before := cur
	cur.Version = version
	if p, present := strPtr(args, "first_name"); present && p != nil {
		cur.FirstName = *p
	}
	if p, present := strPtr(args, "last_name"); present && p != nil {
		cur.LastName = *p
	}
	if p, present := strPtr(args, "email"); present && p != nil {
		cur.Email = *p
	}
	if p, present := strPtr(args, "position"); present && p != nil {
		cur.Position = *p
	}
	if p, present := strPtr(args, "employment_type"); present {
		if p == nil || *p == "" {
			return nil, nil, validationErr("employment_type", "must be one of full_time, part_time, contract, intern")
		}
		et := store.EmploymentType(*p)
		if !store.ValidEmploymentType(et) {
			return nil, nil, validationErr("employment_type", "must be one of full_time, part_time, contract, intern")
		}
		cur.EmploymentType = et
	}
	if p, present := strPtr(args, "group_id"); present {
		cur.GroupID = p
	}
	if p, present := strPtr(args, "manager_id"); present {
		cur.ManagerID = p
	}
	if p, present := strPtr(args, "time_zone"); present && p != nil {
		cur.TimeZone = *p
	}
	if ds := optString(args, "work_start_date"); ds != "" {
		t, err := time.Parse("2006-01-02", ds)
		if err != nil {
			return nil, nil, validationErr("work_start_date", "must be YYYY-MM-DD")
		}
		utc := t.UTC()
		cur.WorkStartDate = &utc
	}
	updated, err := s.store.UpdateEmployee(ctx, cur)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"employee": updated},
		&auditRecord{action: "employee.updated", resourceType: "employee", resourceID: updated.ID, before: before, after: updated}, nil
}

func (s *Server) handleDeactivateEmployee(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	employeeID, terr := reqString(args, "employee_id")
	if terr != nil {
		return nil, nil, terr
	}
	version, terr := reqVersion(args)
	if terr != nil {
		return nil, nil, terr
	}
	before, err := s.store.GetEmployee(ctx, ai.OrgID, employeeID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	updated, err := s.store.DeactivateEmployee(ctx, ai.OrgID, employeeID, version)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"employee": updated},
		&auditRecord{action: "employee.deactivated", resourceType: "employee", resourceID: employeeID, before: before, after: updated}, nil
}

func (s *Server) handleListDepartments(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	p, terr := parsePageArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	groups, next, err := s.store.ListGroups(ctx, ai.OrgID, p)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if groups == nil {
		groups = []store.Group{}
	}
	return pageEnvelope(groups, next),
		&auditRecord{action: "group.listed", resourceType: "group"}, nil
}

func (s *Server) handleGetOrgChart(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	// Page internally; the chart is one document, not a paginated list.
	groups, err := listAllGroups(ctx, s.store, ai.OrgID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	emps, err := listAllEmployees(ctx, s.store, ai.OrgID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	memberCount := map[string]int{}
	for _, e := range emps {
		if e.GroupID != nil && *e.GroupID != "" {
			memberCount[*e.GroupID]++
		}
	}
	groupNodes := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		groupNodes = append(groupNodes, map[string]any{
			"id": g.ID, "name": g.Name, "parent_id": g.ParentID,
			"member_count": memberCount[g.ID],
		})
	}
	reports := map[string][]string{}
	for _, e := range emps {
		if e.ManagerID != nil && *e.ManagerID != "" {
			reports[*e.ManagerID] = append(reports[*e.ManagerID], e.ID)
		}
	}
	empNodes := make([]map[string]any, 0, len(emps))
	var roots []string
	for _, e := range emps {
		dr := reports[e.ID]
		if dr == nil {
			dr = []string{}
		}
		empNodes = append(empNodes, map[string]any{
			"id": e.ID, "name": strings.TrimSpace(e.FirstName + " " + e.LastName),
			"position": e.Position, "manager_id": e.ManagerID, "group_id": e.GroupID,
			"direct_reports": dr,
		})
		if e.ManagerID == nil || *e.ManagerID == "" {
			roots = append(roots, e.ID)
		}
	}
	if roots == nil {
		roots = []string{}
	}
	return map[string]any{"groups": groupNodes, "employees": empNodes, "roots": roots},
		&auditRecord{action: "org_chart.read", resourceType: "organization", resourceID: ai.OrgID}, nil
}

func listAllGroups(ctx context.Context, st store.Store, orgID string) ([]store.Group, error) {
	var all []store.Group
	cursor := ""
	for {
		batch, next, err := st.ListGroups(ctx, orgID, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if next == "" || len(all) >= 5000 {
			break
		}
		cursor = next
	}
	return all, nil
}

func listAllEmployees(ctx context.Context, st store.Store, orgID string) ([]store.Employee, error) {
	var all []store.Employee
	cursor := ""
	for {
		batch, next, err := st.ListEmployees(ctx, orgID, store.EmployeeFilter{}, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if next == "" || len(all) >= 5000 {
			break
		}
		cursor = next
	}
	return all, nil
}

// --- clock tools ---

// clockOp is the shared body of the four clock tools, mirroring the REST
// clockOp: resolve the employee, build the entry, and run the store's clock
// state machine. An optional idempotency_key gives the same double-punch
// protection the REST Idempotency-Key header provides: a retry with the
// same key and identical arguments replays the first result.
func (s *Server) clockOp(ctx context.Context, ai *AuthInfo, toolName string, args map[string]any,
	op func(ctx context.Context, orgID, employeeID string, e store.TimeEntry) (store.TimeEntry, error),
	auditAction string,
) (any, *auditRecord, *ToolError) {
	employeeID, terr := timeEmployee(ai, optString(args, "employee_id"))
	if terr != nil {
		return nil, nil, terr
	}
	occurredAt, _, terr := parseTimeArg(args, "occurred_at")
	if terr != nil {
		return nil, nil, terr
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	entry := store.TimeEntry{OccurredAt: occurredAt}
	if p, present := strPtr(args, "project_id"); present {
		entry.ProjectID = p
	}
	if f, terr := floatPtr(args, "latitude"); terr != nil {
		return nil, nil, terr
	} else {
		entry.Latitude = f
	}
	if f, terr := floatPtr(args, "longitude"); terr != nil {
		return nil, nil, terr
	} else {
		entry.Longitude = f
	}
	if p, present := strPtr(args, "address"); present {
		entry.Address = p
	}
	if ct := strings.TrimSpace(optString(args, "client_type")); ct != "" {
		entry.ClientType = ct
	}

	key := optString(args, "idempotency_key")
	if key != "" {
		return s.clockOpIdempotent(ctx, ai, toolName, key, args, employeeID, entry, op, auditAction)
	}
	created, err := op(ctx, ai.OrgID, employeeID, entry)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"time_entry": created},
		&auditRecord{action: auditAction, resourceType: "time_entry", resourceID: created.ID, after: created}, nil
}

// clockOpIdempotent wraps a clock operation with the store's idempotency
// records, mirroring the REST withIdempotency middleware: the fingerprint
// is hex(sha256(tool|canonicalArgs)) and only successful results are
// cached (errors stay retryable).
func (s *Server) clockOpIdempotent(ctx context.Context, ai *AuthInfo, toolName, key string, args map[string]any,
	employeeID string, entry store.TimeEntry,
	op func(ctx context.Context, orgID, employeeID string, e store.TimeEntry) (store.TimeEntry, error),
	auditAction string,
) (any, *auditRecord, *ToolError) {
	fpArgs := map[string]any{}
	for k, v := range args {
		if k == "idempotency_key" {
			continue
		}
		fpArgs[k] = v
	}
	raw, _ := json.Marshal(fpArgs)
	sum := sha256.Sum256([]byte(toolName + "|" + string(raw)))
	fingerprint := hex.EncodeToString(sum[:])
	idemKey := "mcp:" + key

	if rec, found, err := s.store.IdempotencyGet(ctx, ai.OrgID, idemKey); err != nil {
		return nil, nil, mapStoreError(err)
	} else if found {
		if rec.Fingerprint != fingerprint {
			return nil, nil, toolErr("IDEMPOTENCY_KEY_REUSED", "idempotency key was already used with different arguments")
		}
		var cached map[string]any
		if err := json.Unmarshal(rec.ResponseBody, &cached); err != nil {
			return nil, nil, toolErr("INTERNAL", "internal error")
		}
		// The original call was already audited; a replay must not
		// double-audit.
		return cached, nil, nil
	}

	created, err := op(ctx, ai.OrgID, employeeID, entry)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	result := map[string]any{"time_entry": created}
	if body, err := json.Marshal(result); err == nil {
		_ = s.store.IdempotencyPut(ctx, store.IdempotencyRecord{
			OrganizationID: ai.OrgID,
			Key:            idemKey,
			Fingerprint:    fingerprint,
			StatusCode:     200,
			ResponseBody:   body,
			CreatedAt:      time.Now().UTC(),
		})
	}
	return result,
		&auditRecord{action: auditAction, resourceType: "time_entry", resourceID: created.ID, after: created}, nil
}

func (s *Server) handleClockIn(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.clockOp(ctx, ai, "hris.clock_in", args, s.store.ClockIn, "time_entry.clock_in")
}

func (s *Server) handleClockOut(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.clockOp(ctx, ai, "hris.clock_out", args, s.store.ClockOut, "time_entry.clock_out")
}

func (s *Server) handleStartBreak(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.clockOp(ctx, ai, "hris.start_break", args, s.store.StartBreak, "time_entry.break_start")
}

func (s *Server) handleEndBreak(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.clockOp(ctx, ai, "hris.end_break", args, s.store.EndBreak, "time_entry.break_end")
}

func (s *Server) handleGetTimesheet(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	periodID, terr := reqString(args, "period_id")
	if terr != nil {
		return nil, nil, terr
	}
	period, err := s.store.GetPayPeriod(ctx, ai.OrgID, periodID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	employeeID, terr := timeEmployee(ai, optString(args, "employee_id"))
	if terr != nil {
		return nil, nil, terr
	}
	entries, terr := listEntriesInRange(ctx, s.store, ai.OrgID, employeeID, period.PeriodStart, period.PeriodEnd)
	if terr != nil {
		return nil, nil, terr
	}
	open, isOpen, err := s.store.OpenTimeEntry(ctx, ai.OrgID, employeeID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	var openEntry any
	if isOpen {
		openEntry = open
	}
	return map[string]any{
		"pay_period": period, "entries": entries,
		"open_entry": openEntry, "employee_id": employeeID,
	}, &auditRecord{action: "pay_period.read", resourceType: "pay_period", resourceID: period.ID}, nil
}

func listEntriesInRange(ctx context.Context, st store.Store, orgID, employeeID string, from, to time.Time) ([]store.TimeEntry, *ToolError) {
	var all []store.TimeEntry
	cursor := ""
	for {
		batch, next, err := st.ListTimeEntries(ctx, orgID, store.TimeEntryFilter{
			EmployeeID: employeeID, From: &from, To: &to,
		}, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, mapStoreError(err)
		}
		all = append(all, batch...)
		if next == "" {
			break
		}
		cursor = next
	}
	if all == nil {
		all = []store.TimeEntry{}
	}
	return all, nil
}

func (s *Server) handleListTimesheets(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	f := store.PayPeriodFilter{DefinitionID: optString(args, "definition_id")}
	if st := optString(args, "status"); st != "" {
		f.Status = store.PayPeriodStatus(st)
		if !store.ValidPayPeriodStatus(f.Status) {
			return nil, nil, validationErr("status", "must be one of Open, Submitted, Approved, Rejected, Reopened")
		}
	}
	p, terr := parsePageArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	periods, next, err := s.store.ListPayPeriods(ctx, ai.OrgID, f, p)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if periods == nil {
		periods = []store.PayPeriod{}
	}
	return pageEnvelope(periods, next),
		&auditRecord{action: "pay_period.listed", resourceType: "pay_period"}, nil
}

// timesheetTransition is the shared body of submit/approve/reopen,
// mirroring the REST payPeriodTransition (version-checked).
func (s *Server) timesheetTransition(ctx context.Context, ai *AuthInfo, args map[string]any,
	op func(ctx context.Context, orgID, id string, version int64) (store.PayPeriod, error),
	auditAction string,
) (any, *auditRecord, *ToolError) {
	periodID, terr := reqString(args, "period_id")
	if terr != nil {
		return nil, nil, terr
	}
	version, terr := reqVersion(args)
	if terr != nil {
		return nil, nil, terr
	}
	before, err := s.store.GetPayPeriod(ctx, ai.OrgID, periodID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	updated, err := op(ctx, ai.OrgID, periodID, version)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"pay_period": updated},
		&auditRecord{action: auditAction, resourceType: "pay_period", resourceID: updated.ID, before: before, after: updated}, nil
}

func (s *Server) handleSubmitTimesheet(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.timesheetTransition(ctx, ai, args, s.store.SubmitPayPeriod, "pay_period.submitted")
}

func (s *Server) handleApproveTimesheet(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.timesheetTransition(ctx, ai, args, s.store.ApprovePayPeriod, "pay_period.approved")
}

func (s *Server) handleReopenTimesheet(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.timesheetTransition(ctx, ai, args, s.store.ReopenPayPeriod, "pay_period.reopened")
}

func (s *Server) handleCorrectTimeEntry(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	entryID, terr := reqString(args, "entry_id")
	if terr != nil {
		return nil, nil, terr
	}
	kind, terr := reqString(args, "kind")
	if terr != nil {
		return nil, nil, terr
	}
	if !store.ValidTimeEntryKind(store.TimeEntryKind(kind)) {
		return nil, nil, validationErr("kind", "must be one of in, out, start_break, end_break")
	}
	orig, err := s.store.GetTimeEntry(ctx, ai.OrgID, entryID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if _, terr := timeEmployee(ai, orig.EmployeeID); terr != nil {
		return nil, nil, terr
	}
	correction := store.TimeEntry{Kind: store.TimeEntryKind(kind)}
	if t, ok, terr := parseTimeArg(args, "occurred_at"); terr != nil {
		return nil, nil, terr
	} else if ok {
		correction.OccurredAt = t
	}
	// A zero OccurredAt falls back to the original's inside the store.
	if p, present := strPtr(args, "project_id"); present {
		correction.ProjectID = p
	}
	if f, terr := floatPtr(args, "latitude"); terr != nil {
		return nil, nil, terr
	} else {
		correction.Latitude = f
	}
	if f, terr := floatPtr(args, "longitude"); terr != nil {
		return nil, nil, terr
	} else {
		correction.Longitude = f
	}
	if p, present := strPtr(args, "address"); present {
		correction.Address = p
	}
	if ct := strings.TrimSpace(optString(args, "client_type")); ct != "" {
		correction.ClientType = ct
	}
	created, err := s.store.CorrectTimeEntry(ctx, ai.OrgID, entryID, correction)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"time_entry": created},
		&auditRecord{action: "time_entry.corrected", resourceType: "time_entry", resourceID: created.ID, before: orig, after: created}, nil
}

// --- leave tools ---

func (s *Server) handleListLeaveTypes(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	p, terr := parsePageArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	pols, next, err := s.store.ListTimeOffPolicies(ctx, ai.OrgID, p)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if pols == nil {
		pols = []store.TimeOffPolicy{}
	}
	return pageEnvelope(pols, next),
		&auditRecord{action: "leave_policy.listed", resourceType: "leave_policy"}, nil
}

// holidaysForCalendar loads every holiday of a calendar, paging internally
// (mirrors the REST helper).
func (s *Server) holidaysForCalendar(ctx context.Context, orgID, calendarID string) ([]store.Holiday, *ToolError) {
	var all []store.Holiday
	cursor := ""
	for {
		page, next, err := s.store.ListHolidays(ctx, orgID, calendarID, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, mapStoreError(err)
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return all, nil
}

func (s *Server) handleGetLeaveBalance(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	employeeID, terr := reqString(args, "employee_id")
	if terr != nil {
		return nil, nil, terr
	}
	policyID, terr := reqString(args, "policy_id")
	if terr != nil {
		return nil, nil, terr
	}
	if terr := leaveSelfOrManager(ai, employeeID, " to recalculate this balance"); terr != nil {
		return nil, nil, terr
	}
	asOf := time.Now().UTC()
	if t, ok, terr := parseTimeArg(args, "as_of"); terr != nil {
		return nil, nil, terr
	} else if ok {
		asOf = t
	}
	var holidays []store.Holiday
	if calID := optString(args, "holiday_calendar_id"); calID != "" {
		var terr *ToolError
		holidays, terr = s.holidaysForCalendar(ctx, ai.OrgID, calID)
		if terr != nil {
			return nil, nil, terr
		}
	}
	bal, err := s.store.RecalculateLeaveBalance(ctx, ai.OrgID, employeeID, policyID, asOf, holidays)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"balance": bal},
		&auditRecord{action: "leave_balance.recalculated", resourceType: "leave_balance", resourceID: bal.ID, after: bal}, nil
}

func (s *Server) handleRequestLeave(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	employeeID, terr := reqString(args, "employee_id")
	if terr != nil {
		return nil, nil, terr
	}
	policyID, terr := reqString(args, "policy_id")
	if terr != nil {
		return nil, nil, terr
	}
	startDate, terr := parseDateArg(args, "start_date", true)
	if terr != nil {
		return nil, nil, terr
	}
	endDate, terr := parseDateArg(args, "end_date", true)
	if terr != nil {
		return nil, nil, terr
	}
	if terr := leaveSelfOrManager(ai, employeeID, " to request leave for this employee"); terr != nil {
		return nil, nil, terr
	}
	approverIDs, terr := optStringSlice(args, "approver_ids")
	if terr != nil {
		return nil, nil, terr
	}
	// Only managers and above may set an explicit approver chain (mirrors
	// createLeaveRequest).
	if len(approverIDs) > 0 && !roleAtLeast(store.RoleManager, ai) {
		return nil, nil, toolErr("FORBIDDEN", "only managers and above can set an explicit approver chain")
	}
	// Default chain: the employee's manager, when set (mirrors REST).
	approvers := approverIDs
	if len(approvers) == 0 {
		emp, err := s.store.GetEmployee(ctx, ai.OrgID, employeeID)
		if err != nil {
			return nil, nil, mapStoreError(err)
		}
		if emp.ManagerID != nil && *emp.ManagerID != "" {
			approvers = []string{*emp.ManagerID}
		}
	}
	var holidays []store.Holiday
	var calID *string
	if p, present := strPtr(args, "holiday_calendar_id"); present {
		calID = p
	}
	if calID != nil && *calID != "" {
		var terr *ToolError
		holidays, terr = s.holidaysForCalendar(ctx, ai.OrgID, *calID)
		if terr != nil {
			return nil, nil, terr
		}
	}
	created, err := s.store.CreateTimeOffRequest(ctx, store.TimeOffRequest{
		OrganizationID:    ai.OrgID,
		EmployeeID:        employeeID,
		PolicyID:          policyID,
		HolidayCalendarID: calID,
		StartDate:         startDate,
		EndDate:           endDate,
		IsUnpaid:          optBool(args, "is_unpaid"),
		ApproverIDs:       approvers,
	}, holidays)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"request": created},
		&auditRecord{action: "leave_request.created", resourceType: "leave_request", resourceID: created.ID, after: created}, nil
}

func (s *Server) handleCancelLeaveRequest(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	requestID, terr := reqString(args, "request_id")
	if terr != nil {
		return nil, nil, terr
	}
	version, terr := reqVersion(args)
	if terr != nil {
		return nil, nil, terr
	}
	cur, err := s.store.GetTimeOffRequest(ctx, ai.OrgID, requestID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if terr := leaveSelfOrManager(ai, cur.EmployeeID, " to cancel this leave request"); terr != nil {
		return nil, nil, terr
	}
	cancelled, err := s.store.CancelTimeOffRequest(ctx, ai.OrgID, cur.ID, version)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"request": cancelled},
		&auditRecord{action: "leave_request.cancelled", resourceType: "leave_request", resourceID: cancelled.ID, before: cur, after: cancelled}, nil
}

// decideLeave backs approve/reject, mirroring the REST decideLeaveRequest.
func (s *Server) decideLeave(ctx context.Context, ai *AuthInfo, args map[string]any, approved bool, auditAction string) (any, *auditRecord, *ToolError) {
	requestID, terr := reqString(args, "request_id")
	if terr != nil {
		return nil, nil, terr
	}
	version, terr := reqVersion(args)
	if terr != nil {
		return nil, nil, terr
	}
	before, err := s.store.GetTimeOffRequest(ctx, ai.OrgID, requestID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	var approverID string
	if ai.EmployeeID != nil {
		approverID = *ai.EmployeeID
	}
	asAdmin := roleAtLeast(store.RoleAdmin, ai)
	updated, steps, err := s.store.DecideTimeOffApproval(ctx, ai.OrgID, before.ID, approverID, approved, version, asAdmin)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	return map[string]any{"request": updated, "approvals": steps},
		&auditRecord{action: auditAction, resourceType: "leave_request", resourceID: updated.ID, before: before, after: updated}, nil
}

func (s *Server) handleApproveLeave(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.decideLeave(ctx, ai, args, true, "leave_request.approved")
}

func (s *Server) handleRejectLeave(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	return s.decideLeave(ctx, ai, args, false, "leave_request.rejected")
}

func (s *Server) handleBulkApproveLeave(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	ids, terr := optStringSlice(args, "request_ids")
	if terr != nil {
		return nil, nil, terr
	}
	if len(ids) == 0 {
		return nil, nil, validationErr("request_ids", "at least one request id is required")
	}
	var approverID string
	if ai.EmployeeID != nil {
		approverID = *ai.EmployeeID
	}
	asAdmin := roleAtLeast(store.RoleAdmin, ai)
	approved, err := s.store.BulkApproveTimeOffRequests(ctx, ai.OrgID, ids, approverID, asAdmin)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	rids := make([]string, 0, len(approved))
	for _, a := range approved {
		rids = append(rids, a.ID)
	}
	return map[string]any{"approved": approved},
		&auditRecord{action: "leave_request.bulk_approved", resourceType: "leave_request",
			resourceID: strings.Join(rids, ","), after: approved}, nil
}

func (s *Server) handleWhoIsOnLeave(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	date, terr := parseDateArg(args, "date", false)
	if terr != nil {
		return nil, nil, terr
	}
	if date == "" {
		date = time.Now().UTC().Format("2006-01-02")
	}
	type entry struct {
		RequestID    string `json:"request_id"`
		EmployeeID   string `json:"employee_id"`
		EmployeeName string `json:"employee_name,omitempty"`
		PolicyID     string `json:"policy_id"`
		PolicyName   string `json:"policy_name,omitempty"`
		StartDate    string `json:"start_date"`
		EndDate      string `json:"end_date"`
		IsUnpaid     bool   `json:"is_unpaid"`
	}
	entries := []entry{}
	cursor := ""
	for {
		reqs, next, err := s.store.ListApprovedLeaveOnDate(ctx, ai.OrgID, date, store.Page{Limit: 1000, Cursor: cursor})
		if err != nil {
			return nil, nil, mapStoreError(err)
		}
		for _, lr := range reqs {
			e := entry{
				RequestID: lr.ID, EmployeeID: lr.EmployeeID, PolicyID: lr.PolicyID,
				StartDate: lr.StartDate, EndDate: lr.EndDate, IsUnpaid: lr.IsUnpaid,
			}
			// Best-effort enrichment; IDs are always present.
			if emp, err := s.store.GetEmployee(ctx, ai.OrgID, lr.EmployeeID); err == nil {
				e.EmployeeName = strings.TrimSpace(emp.FirstName + " " + emp.LastName)
			}
			if pol, err := s.store.GetTimeOffPolicy(ctx, ai.OrgID, lr.PolicyID); err == nil {
				e.PolicyName = pol.Name
			}
			entries = append(entries, e)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	return map[string]any{"date": date, "entries": entries},
		&auditRecord{action: "leave_request.listed", resourceType: "leave_request"}, nil
}

// --- report tools ---

func (s *Server) handleGetAttendanceReport(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	employeeID, terr := timeEmployee(ai, optString(args, "employee_id"))
	if terr != nil {
		return nil, nil, terr
	}
	fromStr, terr := parseDateArg(args, "from", true)
	if terr != nil {
		return nil, nil, terr
	}
	toStr, terr := parseDateArg(args, "to", true)
	if terr != nil {
		return nil, nil, terr
	}
	from, _ := time.Parse("2006-01-02", fromStr)
	to, _ := time.Parse("2006-01-02", toStr)
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return nil, nil, validationErr("to", "must be after from")
	}
	p, terr := parsePageArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	entries, next, err := s.store.ListTimeEntries(ctx, ai.OrgID, store.TimeEntryFilter{
		EmployeeID: employeeID, From: &from, To: &to,
	}, p)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if entries == nil {
		entries = []store.TimeEntry{}
	}
	open, isOpen, err := s.store.OpenTimeEntry(ctx, ai.OrgID, employeeID)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	var openEntry any
	if isOpen {
		openEntry = open
	}
	env := pageEnvelope(entries, next)
	env["employee_id"] = employeeID
	env["open_entry"] = openEntry
	return env, &auditRecord{action: "attendance_report.generated", resourceType: "report"}, nil
}

func (s *Server) handleGetLeaveReport(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	status := store.TimeOffStatus(optString(args, "status"))
	if status != "" && !store.ValidTimeOffStatus(status) {
		return nil, nil, validationErr("status", "unknown time-off status")
	}
	p, terr := parsePageArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	requests, _, err := s.store.ListTimeOffRequests(ctx, ai.OrgID, store.TimeOffRequestFilter{
		EmployeeID: optString(args, "employee_id"),
		PolicyID:   optString(args, "policy_id"),
		Status:     status,
	}, p)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if requests == nil {
		requests = []store.TimeOffRequest{}
	}
	balances, _, err := s.store.ListLeaveBalances(ctx, ai.OrgID, store.LeaveBalanceFilter{
		EmployeeID: optString(args, "employee_id"),
		PolicyID:   optString(args, "policy_id"),
		Period:     optString(args, "period"),
	}, store.Page{Limit: 1000})
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if balances == nil {
		balances = []store.LeaveBalance{}
	}
	return map[string]any{"requests": requests, "balances": balances},
		&auditRecord{action: "leave_report.generated", resourceType: "report"}, nil
}

func (s *Server) handleGetAuditLog(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	f := store.AuditFilter{
		Action:       optString(args, "action"),
		ResourceType: optString(args, "resource_type"),
	}
	if at := optString(args, "actor_type"); at != "" {
		f.ActorType = store.ActorType(at)
		if !store.ValidActorType(f.ActorType) {
			return nil, nil, validationErr("actor_type", "must be one of human, agent, system, scim")
		}
	}
	if v, ok, terr := parseTimeArg(args, "since"); terr != nil {
		return nil, nil, terr
	} else if ok {
		f.Since = &v
	}
	p, terr := parsePageArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	events, next, err := s.store.ListAuditEvents(ctx, ai.OrgID, f, p)
	if err != nil {
		return nil, nil, mapStoreError(err)
	}
	if events == nil {
		events = []store.AuditEvent{}
	}
	return pageEnvelope(events, next),
		&auditRecord{action: "audit_log.read", resourceType: "audit_event"}, nil
}

// overtimeRulesFromArgs builds store.OvertimeRules from tool arguments,
// defaulting the daily threshold to 8h like the REST preview callers do.
func overtimeRulesFromArgs(args map[string]any) (store.OvertimeRules, *ToolError) {
	rules := store.OvertimeRules{
		DailyThresholdHours:  8,
		DoubleThresholdHours: 0,
		WeeklyThresholdHours: 0,
		OvertimeMult:         1.5,
		DoubleMult:           2,
		RestDayMult:          2,
		HolidayMult:          3,
	}
	for key, dst := range map[string]*float64{
		"daily_threshold_hours":  &rules.DailyThresholdHours,
		"double_threshold_hours": &rules.DoubleThresholdHours,
		"weekly_threshold_hours": &rules.WeeklyThresholdHours,
	} {
		if f, terr := optFloat(args, key); terr != nil {
			return rules, terr
		} else if _, ok := args[key]; ok {
			*dst = f
		}
	}
	return rules, nil
}

func (s *Server) handleGetOvertimeReport(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *auditRecord, *ToolError) {
	employeeID, terr := timeEmployee(ai, optString(args, "employee_id"))
	if terr != nil {
		return nil, nil, terr
	}
	fromStr, terr := parseDateArg(args, "from", true)
	if terr != nil {
		return nil, nil, terr
	}
	toStr, terr := parseDateArg(args, "to", true)
	if terr != nil {
		return nil, nil, terr
	}
	from, _ := time.Parse("2006-01-02", fromStr)
	to, _ := time.Parse("2006-01-02", toStr)
	from, to = from.UTC(), to.UTC()
	if !to.After(from) {
		return nil, nil, validationErr("to", "must be after from")
	}
	rules, terr := overtimeRulesFromArgs(args)
	if terr != nil {
		return nil, nil, terr
	}
	holStrs, terr := optStringSlice(args, "holidays")
	if terr != nil {
		return nil, nil, terr
	}
	holidays := map[string]bool{}
	for _, h := range holStrs {
		if _, err := time.Parse("2006-01-02", h); err != nil {
			return nil, nil, validationErr("holidays", "must be YYYY-MM-DD")
		}
		holidays[h] = true
	}
	entries, terr := listEntriesInRange(ctx, s.store, ai.OrgID, employeeID, from, to)
	if terr != nil {
		return nil, nil, terr
	}
	result := store.ComputeOvertime(entries, rules, holidays)
	return map[string]any{
		"employee_id": employeeID, "from": fromStr, "to": toStr,
		"entries": len(entries), "result": result,
	}, &auditRecord{action: "overtime_report.generated", resourceType: "report"}, nil
}

// --- approval previews (read-only; never mutate) ---

func (s *Server) previewCreateEmployee(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *ToolError) {
	return map[string]any{
		"summary": "Create employee " + optString(args, "first_name") + " " + optString(args, "last_name") +
			" <" + optString(args, "email") + ">",
		"details": args,
	}, nil
}

func (s *Server) previewDeactivateEmployee(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *ToolError) {
	employeeID, terr := reqString(args, "employee_id")
	if terr != nil {
		return nil, terr
	}
	emp, err := s.store.GetEmployee(ctx, ai.OrgID, employeeID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return map[string]any{
		"summary": "Deactivate employee " + strings.TrimSpace(emp.FirstName+" "+emp.LastName) +
			" (" + emp.ID + ", number " + emp.EmployeeNumber + "); status -> removed, record kept for audit",
		"employee_id": emp.ID,
	}, nil
}

func (s *Server) previewTimesheetTransition(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *ToolError) {
	periodID, terr := reqString(args, "period_id")
	if terr != nil {
		return nil, terr
	}
	period, err := s.store.GetPayPeriod(ctx, ai.OrgID, periodID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return map[string]any{
		"summary": "Change timesheet " + period.ID + " (" +
			period.PeriodStart.Format("2006-01-02") + " to " + period.PeriodEnd.Format("2006-01-02") +
			") from " + string(period.Status),
		"period_id": period.ID,
		"status":    string(period.Status),
	}, nil
}

func (s *Server) previewLeaveDecision(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *ToolError) {
	requestID, terr := reqString(args, "request_id")
	if terr != nil {
		return nil, terr
	}
	lr, err := s.store.GetTimeOffRequest(ctx, ai.OrgID, requestID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	steps, _, err := s.store.ListTimeOffApprovals(ctx, ai.OrgID, requestID, store.Page{Limit: 1000})
	if err != nil {
		return nil, mapStoreError(err)
	}
	var current any
	for i := range steps {
		if steps[i].Status == store.ApprovalPending {
			current = steps[i].ApproverEmployeeID
			break
		}
	}
	empName := lr.EmployeeID
	if emp, err := s.store.GetEmployee(ctx, ai.OrgID, lr.EmployeeID); err == nil {
		empName = strings.TrimSpace(emp.FirstName + " " + emp.LastName)
	}
	return map[string]any{
		"summary": "Decide leave request " + lr.ID + " (" + empName + ", " +
			lr.StartDate + " to " + lr.EndDate + ", " + string(lr.Status) + ")",
		"request_id":       lr.ID,
		"status":           string(lr.Status),
		"current_approver": current,
		"chain_length":     len(steps),
	}, nil
}

// bulkPreview mirrors the REST bulk-approve preview branch: per-request
// approvability without mutating.
func (s *Server) bulkPreview(ctx context.Context, ai *AuthInfo, ids []string) ([]map[string]any, *ToolError) {
	var approverID string
	if ai.EmployeeID != nil {
		approverID = *ai.EmployeeID
	}
	asAdmin := roleAtLeast(store.RoleAdmin, ai)
	previews := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		pv := map[string]any{"request_id": id, "can_approve": false}
		lr, err := s.store.GetTimeOffRequest(ctx, ai.OrgID, id)
		if err != nil {
			pv["reason"] = "not found"
			previews = append(previews, pv)
			continue
		}
		pv["status"] = string(lr.Status)
		if lr.Status != store.TimeOffRequested {
			pv["reason"] = "request is " + string(lr.Status)
			previews = append(previews, pv)
			continue
		}
		steps, _, err := s.store.ListTimeOffApprovals(ctx, ai.OrgID, id, store.Page{Limit: 1000})
		if err != nil {
			pv["reason"] = "could not load approval chain"
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
		pv["current_approver"] = current
		switch {
		case current == nil && !asAdmin:
			pv["reason"] = "no pending approval step"
		case current != nil && *current != approverID && !asAdmin:
			pv["reason"] = "not the current approver"
		default:
			pv["can_approve"] = true
		}
		previews = append(previews, pv)
	}
	return previews, nil
}

func (s *Server) previewBulkApproveLeave(ctx context.Context, ai *AuthInfo, args map[string]any) (any, *ToolError) {
	ids, terr := optStringSlice(args, "request_ids")
	if terr != nil {
		return nil, terr
	}
	if len(ids) == 0 {
		return nil, validationErr("request_ids", "at least one request id is required")
	}
	previews, terr := s.bulkPreview(ctx, ai, ids)
	if terr != nil {
		return nil, terr
	}
	approvable, blocked := 0, 0
	for _, pv := range previews {
		if ok, _ := pv["can_approve"].(bool); ok {
			approvable++
		} else {
			blocked++
		}
	}
	return map[string]any{
		"summary": "Bulk-approve " + itoa(len(ids)) + " leave requests: " +
			itoa(approvable) + " approvable, " + itoa(blocked) + " blocked (all-or-nothing)",
		"counts":  map[string]any{"total": len(ids), "approvable": approvable, "blocked": blocked},
		"results": previews,
	}, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
