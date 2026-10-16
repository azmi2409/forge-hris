package api

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/azmi2409/forge-hris/store"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// checkOrg enforces the cross-tenant guard: the {orgID} path parameter must
// equal the caller's organization, otherwise 403.
func checkOrg(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (string, bool) {
	if orgID := r.PathValue("orgID"); orgID != "" && orgID == ai.OrgID {
		return orgID, true
	}
	writeError(w, r, http.StatusForbidden, "FORBIDDEN", "organization mismatch")
	return "", false
}

// requireRoleOr writes 403 unless the caller meets the minimum role.
func requireRoleOr(w http.ResponseWriter, r *http.Request, ai *AuthInfo, min store.Role, what string) bool {
	if requireRole(min, ai) {
		return true
	}
	writeError(w, r, http.StatusForbidden, "FORBIDDEN",
		"requires "+string(min)+" role or higher"+what)
	return false
}

// requireScopeOr writes 403 unless the caller holds the scope.
func requireScopeOr(w http.ResponseWriter, r *http.Request, ai *AuthInfo, scope string) bool {
	if requireScope(scope, ai) {
		return true
	}
	writeError(w, r, http.StatusForbidden, "FORBIDDEN",
		"requires scope "+scope)
	return false
}

// requireRoleOrScope writes 403 unless the caller meets the minimum role or
// holds the scope. Used where a least-privilege service account should be
// able to do what an admin PAT can (e.g. employees:write for SCIM-style
// provisioning) without minting an all-scopes token.
func requireRoleOrScope(w http.ResponseWriter, r *http.Request, ai *AuthInfo, min store.Role, scope, what string) bool {
	if requireRole(min, ai) || requireScope(scope, ai) {
		return true
	}
	writeError(w, r, http.StatusForbidden, "FORBIDDEN",
		"requires "+string(min)+" role or higher, or scope "+scope+what)
	return false
}

// employeeNumberOrGenerate returns the requested employee number, or a
// generated one when the caller didn't supply it. The store requires
// employee_number to be present and unique per organization; callers with
// their own numbering scheme should pass employee_number explicitly instead.
func employeeNumberOrGenerate(w http.ResponseWriter, r *http.Request, req *string) (string, bool) {
	if req != nil && strings.TrimSpace(*req) != "" {
		return strings.TrimSpace(*req), true
	}
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return "", false
	}
	return "EMP-" + strings.ToUpper(hex.EncodeToString(b[:])), true
}

// derefOrEmpty clears a nullable FK: empty string means "unassign".
func derefOrEmpty(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// --- organizations ---

type bootstrapOwnerRequest struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
}

type bootstrapRequest struct {
	Name        string                `json:"name"`
	Slug        string                `json:"slug"`
	CountryCode string                `json:"country_code"`
	Owner       bootstrapOwnerRequest `json:"owner"`
}

// handleBootstrapOrganization creates the first organization, its owner
// employee (status=joined), the owner membership, and a PAT for the owner.
// It is the only unauthenticated mutating route. The token plaintext is
// returned exactly once; only its SHA-256 hash is stored.
func (s *Server) handleBootstrapOrganization(w http.ResponseWriter, r *http.Request) {
	var req bootstrapRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Slug) == "" ||
		strings.TrimSpace(req.CountryCode) == "" ||
		strings.TrimSpace(req.Owner.FirstName) == "" ||
		strings.TrimSpace(req.Owner.LastName) == "" ||
		strings.TrimSpace(req.Owner.Email) == "" ||
		!strings.Contains(req.Owner.Email, "@") {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"name, slug, country_code and owner{first_name,last_name,email} are required")
		return
	}
	ctx := r.Context()

	org, err := s.store.CreateOrganization(ctx, store.Organization{
		Name: req.Name, Slug: req.Slug, CountryCode: req.CountryCode,
	})
	if !mapError(w, r, err) {
		return
	}
	empNumber, ok := employeeNumberOrGenerate(w, r, nil)
	if !ok {
		return
	}
	emp, err := s.store.CreateEmployee(ctx, store.Employee{
		OrganizationID: org.ID,
		EmployeeNumber: empNumber,
		FirstName:      req.Owner.FirstName,
		LastName:       req.Owner.LastName,
		Email:          req.Owner.Email,
		Status:         store.InvitationJoined,
	})
	if !mapError(w, r, err) {
		return
	}
	if err := s.store.SetRole(ctx, org.ID, emp.ID, store.RoleOwner, emp.ID); !mapError(w, r, err) {
		return
	}

	plaintext, hash, err := newToken()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	cred, err := s.store.IssueCredential(ctx, store.Credential{
		OrganizationID: org.ID,
		Kind:           store.CredentialPAT,
		Name:           "owner bootstrap token",
		TokenHash:      hash,
		EmployeeID:     &emp.ID,
	}, plaintext)
	if !mapError(w, r, err) {
		return
	}

	s.appendAuditSystem(ctx, org.ID, "organization.created", "organization", org.ID, nil, org)
	s.appendAuditSystem(ctx, org.ID, "employee.created", "employee", emp.ID, nil, emp)
	s.appendAuditSystem(ctx, org.ID, "credential.created", "credential", cred.ID, nil, toCredentialResponse(cred))

	writeJSON(w, http.StatusCreated, map[string]any{
		"organization": org,
		"employee":     emp,
		"credential":   toCredentialResponse(cred),
		// Shown once. It cannot be retrieved again; only the hash is stored.
		"token": plaintext,
	})
}

// handleListOrganizations returns the caller's own organization. There is no
// cross-tenant listing: a credential is bound to exactly one organization.
func (s *Server) handleListOrganizations(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	org, err := s.store.GetOrganization(r.Context(), ai.OrgID)
	if !mapError(w, r, err) {
		return
	}
	writePage(w, []store.Organization{org}, "")
}

func (s *Server) handleGetOrganization(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	org, err := s.store.GetOrganization(r.Context(), orgID)
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, org)
}

type patchOrganizationRequest struct {
	Name    *string `json:"name,omitempty"`
	Version int64   `json:"version"`
}

func (s *Server) patchOrganization(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to update the organization") {
		return zero, false
	}
	var req patchOrganizationRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := s.store.GetOrganization(r.Context(), orgID)
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.Name != nil {
		cur.Name = *req.Name
	}
	updated, err := s.store.UpdateOrganization(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

// --- groups ---

type createGroupRequest struct {
	Name     string  `json:"name"`
	ParentID *string `json:"parent_id,omitempty"`
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to create groups") {
		return zero, false
	}
	var req createGroupRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "name: required")
		return zero, false
	}
	created, err := s.store.CreateGroup(r.Context(), store.Group{
		OrganizationID: orgID,
		Name:           req.Name,
		ParentID:       derefOrEmpty(req.ParentID),
	})
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	groups, next, err := s.store.ListGroups(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	if groups == nil {
		groups = []store.Group{}
	}
	writePage(w, groups, next)
}

func (s *Server) handleGetGroup(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	g, err := s.store.GetGroup(r.Context(), orgID, r.PathValue("groupID"))
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, g)
}

type patchGroupRequest struct {
	Name     *string `json:"name,omitempty"`
	ParentID *string `json:"parent_id,omitempty"`
	Version  int64   `json:"version"`
}

func (s *Server) patchGroup(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to update groups") {
		return zero, false
	}
	var req patchGroupRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	cur, err := s.store.GetGroup(r.Context(), orgID, r.PathValue("groupID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.Name != nil {
		cur.Name = *req.Name
	}
	if req.ParentID != nil {
		cur.ParentID = derefOrEmpty(req.ParentID)
	}
	updated, err := s.store.UpdateGroup(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleManager, " to delete groups") {
		return zero, false
	}
	groupID := r.PathValue("groupID")
	before, err := s.store.GetGroup(r.Context(), orgID, groupID)
	if !mapError(w, r, err) {
		return zero, false
	}
	// ErrNotEmpty (members still assigned) maps to 409 GROUP_NOT_EMPTY.
	if err := s.store.DeleteGroup(r.Context(), orgID, groupID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	return auditData{ResourceID: groupID, Before: before}, true
}

// --- employees ---

type createEmployeeRequest struct {
	EmployeeNumber *string    `json:"employee_number,omitempty"`
	FirstName      string     `json:"first_name"`
	LastName       string     `json:"last_name"`
	Email          string     `json:"email"`
	Position       *string    `json:"position,omitempty"`
	EmploymentType *string    `json:"employment_type,omitempty"`
	GroupID        *string    `json:"group_id,omitempty"`
	ManagerID      *string    `json:"manager_id,omitempty"`
	TimeZone       *string    `json:"time_zone,omitempty"`
	WorkStartDate  *time.Time `json:"work_start_date,omitempty"`
}

func (s *Server) createEmployee(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopeEmployeesWrite, " to create employees") {
		return zero, false
	}
	var req createEmployeeRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if strings.TrimSpace(req.FirstName) == "" || strings.TrimSpace(req.LastName) == "" ||
		strings.TrimSpace(req.Email) == "" || !strings.Contains(req.Email, "@") {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"first_name, last_name and a valid email are required")
		return zero, false
	}
	emp := store.Employee{
		OrganizationID: orgID,
		FirstName:      req.FirstName,
		LastName:       req.LastName,
		Email:          req.Email,
		GroupID:        derefOrEmpty(req.GroupID),
		ManagerID:      derefOrEmpty(req.ManagerID),
	}
	empNumber, ok := employeeNumberOrGenerate(w, r, req.EmployeeNumber)
	if !ok {
		return zero, false
	}
	emp.EmployeeNumber = empNumber
	if req.Position != nil {
		emp.Position = *req.Position
	}
	if req.EmploymentType != nil && *req.EmploymentType != "" {
		et := store.EmploymentType(*req.EmploymentType)
		if !store.ValidEmploymentType(et) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"employment_type must be one of full_time, part_time, contract, intern")
			return zero, false
		}
		emp.EmploymentType = et
	}
	if req.TimeZone != nil {
		emp.TimeZone = *req.TimeZone
	}
	emp.WorkStartDate = req.WorkStartDate
	// Status is left at the store default (the invitation lifecycle);
	// only the bootstrap owner is created already joined.

	created, err := s.store.CreateEmployee(r.Context(), emp)
	if !mapError(w, r, err) {
		return zero, false
	}
	// New hires start as members once joined; promotion goes through
	// PUT /roles. (The store only grants roles to joined employees.)
	if created.Status == store.InvitationJoined {
		if err := s.store.SetRole(r.Context(), orgID, created.ID, store.RoleMember, grantedByOf(ai)); !mapError(w, r, err) {
			return zero, false
		}
	}
	writeJSON(w, http.StatusCreated, created)
	s.emitWebhook(r.Context(), orgID, store.WebhookEventEmployeeCreated, created)
	return auditData{ResourceID: created.ID, After: created}, true
}

func grantedByOf(ai *AuthInfo) string {
	if ai.EmployeeID != nil && *ai.EmployeeID != "" {
		return *ai.EmployeeID
	}
	return ai.CredentialID
}

func (s *Server) handleListEmployees(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeEmployeesRead) {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := store.EmployeeFilter{
		GroupID: q.Get("group_id"),
		Status:  store.InvitationStatus(q.Get("status")),
		Search:  q.Get("q"),
	}
	if q.Get("status") != "" && !store.ValidInvitationStatus(filter.Status) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"status must be one of not_invited, pending_invitation, joined, removed")
		return
	}
	employees, next, err := s.store.ListEmployees(r.Context(), orgID, filter, page)
	if !mapError(w, r, err) {
		return
	}
	if employees == nil {
		employees = []store.Employee{}
	}
	writePage(w, employees, next)
}

// handleGetEmployee reads one employee. Members may only read their own
// record; everyone else with employees:read may read any record in the org.
func (s *Server) handleGetEmployee(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeEmployeesRead) {
		return
	}
	employeeID := r.PathValue("employeeID")
	if effectiveRole(ai) == store.RoleMember &&
		(ai.EmployeeID == nil || *ai.EmployeeID != employeeID) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN",
			"members may only read their own employee record")
		return
	}
	emp, err := s.store.GetEmployee(r.Context(), orgID, employeeID)
	if !mapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, emp)
}

type patchEmployeeRequest struct {
	FirstName      *string    `json:"first_name,omitempty"`
	LastName       *string    `json:"last_name,omitempty"`
	Email          *string    `json:"email,omitempty"`
	Position       *string    `json:"position,omitempty"`
	EmploymentType *string    `json:"employment_type,omitempty"`
	GroupID        *string    `json:"group_id,omitempty"`
	ManagerID      *string    `json:"manager_id,omitempty"`
	TimeZone       *string    `json:"time_zone,omitempty"`
	WorkStartDate  *time.Time `json:"work_start_date,omitempty"`
	Version        int64      `json:"version"`
}

func (s *Server) patchEmployee(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopeEmployeesWrite, " to update employees") {
		return zero, false
	}
	var req patchEmployeeRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	if req.Email != nil && !strings.Contains(*req.Email, "@") {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "email: invalid")
		return zero, false
	}
	cur, err := s.store.GetEmployee(r.Context(), orgID, r.PathValue("employeeID"))
	if !mapError(w, r, err) {
		return zero, false
	}
	before := cur
	cur.Version = req.Version
	if req.FirstName != nil {
		cur.FirstName = *req.FirstName
	}
	if req.LastName != nil {
		cur.LastName = *req.LastName
	}
	if req.Email != nil {
		cur.Email = *req.Email
	}
	if req.Position != nil {
		cur.Position = *req.Position
	}
	if req.EmploymentType != nil {
		if *req.EmploymentType == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"employment_type must be one of full_time, part_time, contract, intern")
			return zero, false
		}
		et := store.EmploymentType(*req.EmploymentType)
		if !store.ValidEmploymentType(et) {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"employment_type must be one of full_time, part_time, contract, intern")
			return zero, false
		}
		cur.EmploymentType = et
	}
	if req.GroupID != nil {
		cur.GroupID = derefOrEmpty(req.GroupID)
	}
	if req.ManagerID != nil {
		cur.ManagerID = derefOrEmpty(req.ManagerID)
	}
	if req.TimeZone != nil {
		cur.TimeZone = *req.TimeZone
	}
	if req.WorkStartDate != nil {
		cur.WorkStartDate = req.WorkStartDate
	}
	updated, err := s.store.UpdateEmployee(r.Context(), cur)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	s.emitWebhook(r.Context(), orgID, store.WebhookEventEmployeeUpdated, updated)
	return auditData{ResourceID: updated.ID, Before: before, After: updated}, true
}

type deactivateEmployeeRequest struct {
	Version int64 `json:"version"`
}

func (s *Server) deactivateEmployee(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOrScope(w, r, ai, store.RoleAdmin, store.ScopeEmployeesWrite, " to deactivate employees") {
		return zero, false
	}
	var req deactivateEmployeeRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	if req.Version <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "version: required and must be positive")
		return zero, false
	}
	employeeID := r.PathValue("employeeID")
	before, err := s.store.GetEmployee(r.Context(), orgID, employeeID)
	if !mapError(w, r, err) {
		return zero, false
	}
	updated, err := s.store.DeactivateEmployee(r.Context(), orgID, employeeID, req.Version)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, updated)
	s.emitWebhook(r.Context(), orgID, store.WebhookEventEmployeeDeactivated, updated)
	return auditData{ResourceID: employeeID, Before: before, After: updated}, true
}

// --- roles ---

type setRoleRequest struct {
	Role string `json:"role"`
}

// setRole assigns a role. Owners may assign any role; admins may only assign
// manager/member — an admin cannot create another admin or owner.
func (s *Server) setRole(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to assign roles") {
		return zero, false
	}
	var req setRoleRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	role := store.Role(req.Role)
	if !store.ValidRole(role) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"role must be one of owner, admin, manager, member")
		return zero, false
	}
	if effectiveRole(ai) == store.RoleAdmin &&
		(role == store.RoleOwner || role == store.RoleAdmin) {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN",
			"admins may only assign manager and member roles")
		return zero, false
	}
	employeeID := r.PathValue("employeeID")
	if _, err := s.store.GetEmployee(r.Context(), orgID, employeeID); !mapError(w, r, err) {
		return zero, false
	}
	var beforeRole *store.Role
	if cur, err := s.store.GetRole(r.Context(), orgID, employeeID); err == nil {
		beforeRole = &cur
	} else if !isNotFound(err) {
		mapError(w, r, err)
		return zero, false
	}
	if err := s.store.SetRole(r.Context(), orgID, employeeID, role, grantedByOf(ai)); !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusOK, map[string]any{"employee_id": employeeID, "role": role})
	return auditData{
		ResourceID: employeeID,
		Before:     map[string]any{"role": roleOrNil(beforeRole)},
		After:      map[string]any{"role": role},
	}, true
}

func roleOrNil(r *store.Role) any {
	if r == nil {
		return nil
	}
	return *r
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

func (s *Server) handleListMemberships(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to list roles") {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	memberships, next, err := s.store.ListMemberships(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	if memberships == nil {
		memberships = []store.Membership{}
	}
	writePage(w, memberships, next)
}

// --- credentials ---

// credentialResponse is the public shape of a credential. The token hash is
// never exposed; the plaintext token appears only in the issuance response.
type credentialResponse struct {
	ID             string               `json:"id"`
	OrganizationID string               `json:"organization_id"`
	Kind           store.CredentialKind `json:"kind"`
	Name           string               `json:"name"`
	EmployeeID     *string              `json:"employee_id,omitempty"`
	Scopes         []string             `json:"scopes"`
	ExpiresAt      *time.Time           `json:"expires_at,omitempty"`
	RevokedAt      *time.Time           `json:"revoked_at,omitempty"`
	LastUsedAt     *time.Time           `json:"last_used_at,omitempty"`
	CreatedAt      time.Time            `json:"created_at"`
	Version        int64                `json:"version"`
}

func toCredentialResponse(c store.Credential) credentialResponse {
	return credentialResponse{
		ID:             c.ID,
		OrganizationID: c.OrganizationID,
		Kind:           c.Kind,
		Name:           c.Name,
		EmployeeID:     c.EmployeeID,
		Scopes:         c.Scopes,
		ExpiresAt:      c.ExpiresAt,
		RevokedAt:      c.RevokedAt,
		LastUsedAt:     c.LastUsedAt,
		CreatedAt:      c.CreatedAt,
		Version:        c.Version,
	}
}

type createCredentialRequest struct {
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	EmployeeID *string    `json:"employee_id,omitempty"`
	Scopes     []string   `json:"scopes,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// issueCredential mints a PAT (bound to a joined employee; scopes derived
// from the employee's role) or a service account (explicit validated scopes).
// The token plaintext is returned exactly once.
func (s *Server) issueCredential(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to issue credentials") {
		return zero, false
	}
	var req createCredentialRequest
	if !decodeJSON(w, r, &req) {
		return zero, false
	}
	kind := store.CredentialKind(req.Kind)
	if !store.ValidCredentialKind(kind) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"kind must be one of pat, service_account")
		return zero, false
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "name: required")
		return zero, false
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "expires_at must be in the future")
		return zero, false
	}

	var employeeID *string
	var scopes []string
	switch kind {
	case store.CredentialPAT:
		if req.EmployeeID == nil || *req.EmployeeID == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"employee_id: required for PAT credentials")
			return zero, false
		}
		if len(req.Scopes) > 0 {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"scopes: must not be set for PAT credentials; scopes are derived from the employee's role")
			return zero, false
		}
		emp, err := s.store.GetEmployee(r.Context(), orgID, *req.EmployeeID)
		if !mapError(w, r, err) {
			return zero, false
		}
		if emp.Status != store.InvitationJoined {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"employee_id: employee must have joined status to hold a PAT")
			return zero, false
		}
		employeeID = &emp.ID
	case store.CredentialServiceAccount:
		if len(req.Scopes) == 0 {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"scopes: at least one scope is required for service accounts")
			return zero, false
		}
		for _, sc := range req.Scopes {
			if !store.ValidScope(sc) {
				writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
					"scopes: unknown scope "+sc)
				return zero, false
			}
		}
		scopes = append([]string(nil), req.Scopes...)
	}

	// payroll:submit is never bundled: only an owner-equivalent caller may
	// grant it, per the REA scope rules.
	if containsScope(scopes, store.ScopePayrollSubmit) && effectiveRole(ai) != store.RoleOwner {
		writeError(w, r, http.StatusForbidden, "FORBIDDEN",
			"payroll:submit may only be granted by an owner")
		return zero, false
	}

	plaintext, hash, err := newToken()
	if err != nil {
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", "internal error")
		return zero, false
	}
	cred, err := s.store.IssueCredential(r.Context(), store.Credential{
		OrganizationID: orgID,
		Kind:           kind,
		Name:           req.Name,
		TokenHash:      hash,
		EmployeeID:     employeeID,
		Scopes:         scopes,
		ExpiresAt:      req.ExpiresAt,
	}, plaintext)
	if !mapError(w, r, err) {
		return zero, false
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"credential": toCredentialResponse(cred),
		// Shown once. It cannot be retrieved again; only the hash is stored.
		"token": plaintext,
	})
	// The public credential shape carries no token hash; the raw store
	// value must never leave the process in an event payload.
	s.emitWebhook(r.Context(), orgID, store.WebhookEventAPITokenCreated, toCredentialResponse(cred))
	return auditData{ResourceID: cred.ID, After: toCredentialResponse(cred)}, true
}

func containsScope(scopes []string, want string) bool {
	for _, sc := range scopes {
		if sc == want {
			return true
		}
	}
	return false
}

func (s *Server) handleListCredentials(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to list credentials") {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	creds, next, err := s.store.ListCredentials(r.Context(), orgID, page)
	if !mapError(w, r, err) {
		return
	}
	out := make([]credentialResponse, 0, len(creds))
	for _, c := range creds {
		out = append(out, toCredentialResponse(c))
	}
	writePage(w, out, next)
}

func (s *Server) revokeCredential(w http.ResponseWriter, r *http.Request, ai *AuthInfo) (auditData, bool) {
	var zero auditData
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return zero, false
	}
	if !requireRoleOr(w, r, ai, store.RoleAdmin, " to revoke credentials") {
		return zero, false
	}
	credentialID := r.PathValue("credentialID")
	// Fetch the target first for the audit trail; the store read also
	// enforces the org scoping (unknown ID -> 404, not a silent no-op).
	target, err := s.store.GetCredential(r.Context(), orgID, credentialID)
	if !mapError(w, r, err) {
		return zero, false
	}
	if err := s.store.RevokeCredential(r.Context(), orgID, credentialID); !mapError(w, r, err) {
		return zero, false
	}
	w.WriteHeader(http.StatusNoContent)
	s.emitWebhook(r.Context(), orgID, store.WebhookEventAPITokenRevoked,
		map[string]any{"id": credentialID, "organization_id": orgID})
	return auditData{ResourceID: credentialID, Before: toCredentialResponse(target)}, true
}

// --- audit events ---

func (s *Server) handleListAuditEvents(w http.ResponseWriter, r *http.Request) {
	ai, ok := authOr401(w, r)
	if !ok {
		return
	}
	orgID, ok := checkOrg(w, r, ai)
	if !ok {
		return
	}
	if !requireScopeOr(w, r, ai, store.ScopeAuditRead) {
		return
	}
	page, ok := parsePage(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter := store.AuditFilter{
		ActorType:    store.ActorType(q.Get("actor_type")),
		Action:       q.Get("action"),
		ResourceType: q.Get("resource_type"),
	}
	if q.Get("actor_type") != "" && !store.ValidActorType(filter.ActorType) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
			"actor_type must be one of human, agent, system, scim")
		return
	}
	if v := q.Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR",
				"since must be RFC3339, e.g. 2026-10-16T00:00:00Z")
			return
		}
		filter.Since = &t
	}
	events, next, err := s.store.ListAuditEvents(r.Context(), orgID, filter, page)
	if !mapError(w, r, err) {
		return
	}
	if events == nil {
		events = []store.AuditEvent{}
	}
	writePage(w, events, next)
}
