// SCIM 2.0 provisioning surface (forge-hris component 4, Worker C).
//
// Implements the /scim/v2/Users and /scim/v2/Groups endpoints per RFC 7643 /
// RFC 7644 so an external IdP (Entra ID, Okta, Google Workspace) can
// provision and deprovision employees and groups.
//
// Design notes:
//   - Mounted at /scim/v2/..., NOT under /api/v1: SCIM clients address a
//     fixed base URL. The tenant comes from the credential, not the path —
//     service-account credentials are tenant-scoped (one token per company).
//   - Auth is a dedicated middleware in this file: Bearer token ->
//     sha256 hex -> store.GetCredentialByTokenHash. Only service accounts
//     are accepted (PATs get 401); reads need employees:read, mutations need
//     employees:write (least-privilege; no all-scopes token required).
//   - DELETE on a user is a soft-deactivate (active=false semantics via
//     DeactivateEmployee); employees are never hard-deleted, and the
//     externalId link is kept so the IdP can re-correlate on re-provision.
//   - Group membership maps onto the employee's single department GroupID:
//     the core store has no multi-group membership table, so a SCIM group
//     member is an employee whose group_id points at the group.
//   - Errors use the SCIM error body (not the /api/v1 envelope) with the
//     HTTP status mirrored in the string "status" field, per RFC 7644 3.12.
//   - JSON decoding is lenient on purpose: IdPs send "schemas" members,
//     enterprise-extension blocks, and other attributes we don't model;
//     rejecting unknown fields would break real provisioning flows.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/azmi2409/forge-hris/store"
)

// SCIM schema URNs (RFC 7643 / RFC 7644). These exact strings are the
// wire contract — IdPs match on them.
const (
	scimSchemaUser         = "urn:ietf:params:scim:schemas:core:2.0:User"
	scimSchemaGroup        = "urn:ietf:params:scim:schemas:core:2.0:Group"
	scimSchemaListResponse = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	scimSchemaError        = "urn:ietf:params:scim:api:messages:2.0:Error"
	scimSchemaPatchOp      = "urn:ietf:params:scim:api:messages:2.0:PatchOp"
)

// --- resource representations ---

type scimName struct {
	GivenName  string `json:"givenName"`
	FamilyName string `json:"familyName,omitempty"`
}

type scimEmail struct {
	Value   string `json:"value"`
	Primary bool   `json:"primary,omitempty"`
	Type    string `json:"type,omitempty"`
}

type scimMeta struct {
	ResourceType string `json:"resourceType"`
	Created      string `json:"created"`
	LastModified string `json:"lastModified"`
}

// scimUser is the SCIM User resource. userName is the employee email (the
// unique login identifier); externalId comes from the scim_links table.
type scimUser struct {
	Schemas    []string    `json:"schemas"`
	ID         string      `json:"id"`
	UserName   string      `json:"userName"`
	ExternalID *string     `json:"externalId,omitempty"`
	Name       scimName    `json:"name"`
	Emails     []scimEmail `json:"emails"`
	Active     bool        `json:"active"`
	Meta       scimMeta    `json:"meta"`
}

type scimGroupMember struct {
	Value   string `json:"value"`
	Ref     string `json:"$ref,omitempty"`
	Display string `json:"display,omitempty"`
}

// scimGroup is the SCIM Group resource. Members are employees whose group_id
// points at the group (see package doc).
type scimGroup struct {
	Schemas     []string          `json:"schemas"`
	ID          string            `json:"id"`
	DisplayName string            `json:"displayName"`
	Members     []scimGroupMember `json:"members"`
	Meta        scimMeta          `json:"meta"`
}

type scimListResponse struct {
	Schemas      []string `json:"schemas"`
	TotalResults int      `json:"totalResults"`
	StartIndex   int      `json:"startIndex"`
	ItemsPerPage int      `json:"itemsPerPage"`
	Resources    []any    `json:"Resources"`
}

// scimError is the RFC 7644 error body. Status is a string on the wire.
type scimError struct {
	Schemas  []string `json:"schemas"`
	Status   string   `json:"status"`
	ScimType string   `json:"scimType,omitempty"`
	Detail   string   `json:"detail"`
}

// scimPatchOp is one entry of a PATCH Operations array. Value is raw: its
// shape depends on Path.
type scimPatchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

// --- request shapes (lenient: unknown fields ignored) ---

type scimUserRequest struct {
	UserName   string  `json:"userName"`
	ExternalID *string `json:"externalId"`
	Name       struct {
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"name"`
	Emails []scimEmail `json:"emails"`
	Active *bool       `json:"active"`
}

type scimPatchRequest struct {
	Operations []scimPatchOp `json:"Operations"`
	OpsLower   []scimPatchOp `json:"operations"`
}

func (p scimPatchRequest) ops() []scimPatchOp {
	if p.Operations != nil {
		return p.Operations
	}
	return p.OpsLower
}

type scimGroupRequest struct {
	DisplayName string  `json:"displayName"`
	ExternalID  *string `json:"externalId"`
	Members     []struct {
		Value string `json:"value"`
	} `json:"members"`
}

// --- routes ---

// scimRoutes registers the SCIM 2.0 endpoints on the server mux. The
// coordinator wires this into routes() in server.go; it is a separate method
// so no existing file changes.
func (s *Server) scimRoutes() {
	m := s.mux
	m.HandleFunc("GET /scim/v2/Users", s.scimAuth(false, s.scimListUsers))
	m.HandleFunc("POST /scim/v2/Users", s.scimAuth(true, s.scimCreateUser))
	m.HandleFunc("GET /scim/v2/Users/{id}", s.scimAuth(false, s.scimGetUser))
	m.HandleFunc("PUT /scim/v2/Users/{id}", s.scimAuth(true, s.scimReplaceUser))
	m.HandleFunc("PATCH /scim/v2/Users/{id}", s.scimAuth(true, s.scimPatchUser))
	m.HandleFunc("DELETE /scim/v2/Users/{id}", s.scimAuth(true, s.scimDeleteUser))

	m.HandleFunc("GET /scim/v2/Groups", s.scimAuth(false, s.scimListGroups))
	m.HandleFunc("POST /scim/v2/Groups", s.scimAuth(true, s.scimCreateGroup))
	m.HandleFunc("GET /scim/v2/Groups/{id}", s.scimAuth(false, s.scimGetGroup))
	m.HandleFunc("PATCH /scim/v2/Groups/{id}", s.scimAuth(true, s.scimPatchGroup))
	m.HandleFunc("DELETE /scim/v2/Groups/{id}", s.scimAuth(true, s.scimDeleteGroup))
}

// --- dedicated SCIM auth ---

// scimAuth wraps a SCIM handler with the SCIM bearer check. write=false
// gates reads (employees:read); write=true gates mutations
// (employees:write).
func (s *Server) scimAuth(write bool, next func(http.ResponseWriter, *http.Request, *AuthInfo)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ai, ok := s.scimAuthenticate(w, r, write)
		if !ok {
			return
		}
		next(w, r, ai)
	}
}

func (s *Server) scimAuthenticate(w http.ResponseWriter, r *http.Request, write bool) (*AuthInfo, bool) {
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") || strings.TrimSpace(parts[1]) == "" {
		scimWriteError(w, http.StatusUnauthorized, "invalidValue",
			"missing or malformed Authorization header; expected 'Bearer <token>'")
		return nil, false
	}
	// The store rejects unknown, expired, and revoked tokens alike so
	// callers can't distinguish them.
	cred, err := s.store.GetCredentialByTokenHash(r.Context(), hashToken(strings.TrimSpace(parts[1])))
	if err != nil {
		scimWriteError(w, http.StatusUnauthorized, "invalidValue",
			"invalid, expired, or revoked credential")
		return nil, false
	}
	if cred.Kind != store.CredentialServiceAccount {
		// SCIM provisioning is a machine-to-machine flow: personal access
		// tokens are rejected here even though the main /api/v1 auth
		// middleware accepts them.
		scimWriteError(w, http.StatusUnauthorized, "invalidValue",
			"SCIM requires a service account credential")
		return nil, false
	}
	ai := &AuthInfo{
		OrgID:        cred.OrganizationID,
		CredentialID: cred.ID,
		Kind:         cred.Kind,
		Scopes:       append([]string(nil), cred.Scopes...),
	}
	if !requireScope(store.ScopeEmployeesRead, ai) {
		scimWriteError(w, http.StatusForbidden, "invalidValue",
			"service account requires the employees:read scope")
		return nil, false
	}
	if write && !requireScope(store.ScopeEmployeesWrite, ai) {
		// Least-privilege provisioning: SCIM mutations need the
		// employees:write scope (mintable on a service account without
		// granting every scope).
		scimWriteError(w, http.StatusForbidden, "invalidValue",
			"SCIM mutations require the employees:write scope")
		return nil, false
	}
	return ai, true
}

// --- errors ---

// scimWriteError writes the RFC 7644 error body with the HTTP status
// mirrored in the string "status" field.
func scimWriteError(w http.ResponseWriter, status int, scimType, detail string) {
	writeJSON(w, status, scimError{
		Schemas:  []string{scimSchemaError},
		Status:   strconv.Itoa(status),
		ScimType: scimType,
		Detail:   detail,
	})
}

// scimMapError converts store sentinel errors to SCIM error responses.
// ok=false means the error was written; the handler should return.
func scimMapError(w http.ResponseWriter, r *http.Request, err error) bool {
	if err == nil {
		return true
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		scimWriteError(w, http.StatusNotFound, "invalidValue", err.Error())
	case errors.Is(err, store.ErrAlreadyExists):
		scimWriteError(w, http.StatusConflict, "uniqueness", err.Error())
	case errors.Is(err, store.ErrVersionConflict):
		scimWriteError(w, http.StatusPreconditionFailed, "invalidValue",
			"resource changed since it was read; GET it again and retry")
	case errors.Is(err, store.ErrNotEmpty):
		scimWriteError(w, http.StatusConflict, "invalidValue", err.Error())
	default:
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			scimWriteError(w, http.StatusBadRequest, "invalidValue", ve.Error())
		} else {
			scimWriteError(w, http.StatusInternalServerError, "", "internal error")
		}
	}
	return false
}

// scimDecodeJSON decodes a SCIM request body WITHOUT DisallowUnknownFields:
// IdPs routinely send "schemas" members and extension blocks we don't model,
// and strict decoding would break real provisioning flows.
func scimDecodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		scimWriteError(w, http.StatusBadRequest, "invalidSyntax", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// --- shared helpers ---

// scimLinkStore type-asserts the server store to the SCIM link contract.
func (s *Server) scimLinkStore(w http.ResponseWriter, r *http.Request) (store.SCIMLinkStore, bool) {
	ls, ok := s.store.(store.SCIMLinkStore)
	if !ok {
		scimWriteError(w, http.StatusInternalServerError, "", "SCIM store unavailable")
		return nil, false
	}
	return ls, true
}

// scimAudit records a SCIM provisioning event with the first-class scim
// actor type (see the REA agent-ready design: scim is a distinct actor from
// human and agent).
func (s *Server) scimAudit(ctx context.Context, r *http.Request, ai *AuthInfo,
	action, resourceType, resourceID string, before, after any,
) {
	s.recordAudit(ctx, store.AuditEvent{
		OrganizationID: ai.OrgID,
		ActorType:      store.ActorSCIM,
		ActorID:        ai.CredentialID,
		Action:         action,
		ResourceType:   resourceType,
		ResourceID:     resourceID,
		Before:         mustMarshalJSON(before),
		After:          mustMarshalJSON(after),
		IdempotencyKey: idempotencyKeyOf(r),
	})
}

// toSCIMUser renders an employee as a SCIM User resource. externalID is the
// linked IdP identifier, or nil when the employee carries no link.
func toSCIMUser(emp store.Employee, externalID *string) scimUser {
	userName := emp.Email
	if userName == "" {
		userName = emp.EmployeeNumber
	}
	emails := []scimEmail{}
	if emp.Email != "" {
		emails = append(emails, scimEmail{Value: emp.Email, Primary: true})
	}
	return scimUser{
		Schemas:    []string{scimSchemaUser},
		ID:         emp.ID,
		UserName:   userName,
		ExternalID: externalID,
		Name:       scimName{GivenName: emp.FirstName, FamilyName: emp.LastName},
		Emails:     emails,
		Active:     emp.Status == store.InvitationJoined,
		Meta: scimMeta{
			ResourceType: "User",
			Created:      emp.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			LastModified: emp.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		},
	}
}

// scimExternalID returns the employee's linked externalId, or nil.
func (s *Server) scimExternalID(ctx context.Context, ls store.SCIMLinkStore, orgID, employeeID string) *string {
	l, err := ls.GetSCIMLinkByEmployee(ctx, orgID, employeeID)
	if err != nil {
		return nil
	}
	return &l.ExternalID
}

// scimAllEmployees walks the cursor-paginated store list and returns every
// matching employee. SCIM needs totalResults for ListResponse and the store
// exposes no count query, so a full walk is required; provisioning syncs are
// infrequent and the walk is page-bounded.
func (s *Server) scimAllEmployees(ctx context.Context, orgID string, f store.EmployeeFilter) ([]store.Employee, error) {
	var all []store.Employee
	cursor := ""
	for {
		page, next, err := s.store.ListEmployees(ctx, orgID, f, store.Page{Limit: 500, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return all, nil
}

func (s *Server) scimAllGroups(ctx context.Context, orgID string) ([]store.Group, error) {
	var all []store.Group
	cursor := ""
	for {
		page, next, err := s.store.ListGroups(ctx, orgID, store.Page{Limit: 500, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return all, nil
}

// scimPagination reads startIndex (1-based, default 1) and count (default
// 100, max 1000) per RFC 7644 3.4.2.
func scimPagination(w http.ResponseWriter, r *http.Request) (start, count int, ok bool) {
	start, count = 1, 100
	if v := r.URL.Query().Get("startIndex"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			scimWriteError(w, http.StatusBadRequest, "invalidValue", "startIndex must be a positive integer")
			return 0, 0, false
		}
		start = n
	}
	if v := r.URL.Query().Get("count"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			scimWriteError(w, http.StatusBadRequest, "invalidValue", "count must be a non-negative integer")
			return 0, 0, false
		}
		count = n
		if count > 1000 {
			count = 1000
		}
	}
	return start, count, true
}

// writeSCIMList slices resources per startIndex/count and writes the
// ListResponse envelope. Resources must be non-nil so Resources encodes [].
func writeSCIMList(w http.ResponseWriter, resources []any, start, count int) {
	if resources == nil {
		resources = []any{}
	}
	total := len(resources)
	from := start - 1
	if from > total {
		from = total
	}
	to := from + count
	if to > total {
		to = total
	}
	writeJSON(w, http.StatusOK, scimListResponse{
		Schemas:      []string{scimSchemaListResponse},
		TotalResults: total,
		StartIndex:   start,
		ItemsPerPage: to - from,
		Resources:    resources[from:to],
	})
}

// parseSCIMFilter parses the minimal filter dialect we support:
//
//	userName eq "alice@example.com"
//	externalId eq "entra-123"
//
// Attribute names are case-insensitive; only eq is supported.
func parseSCIMFilter(raw string) (attr, value string, ok bool) {
	idx := strings.Index(strings.ToLower(raw), " eq ")
	if idx < 0 {
		return "", "", false
	}
	attr = strings.TrimSpace(raw[:idx])
	value = strings.TrimSpace(raw[idx+4:])
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	if attr == "" || value == "" {
		return "", "", false
	}
	return attr, value, true
}

// scimPrimaryEmail picks the request email: the primary emails[] entry, else
// the first, else the userName.
func scimPrimaryEmail(req scimUserRequest) string {
	email := strings.TrimSpace(req.UserName)
	if len(req.Emails) > 0 {
		first := strings.TrimSpace(req.Emails[0].Value)
		email = first
		for _, e := range req.Emails {
			if e.Primary && strings.TrimSpace(e.Value) != "" {
				email = strings.TrimSpace(e.Value)
				break
			}
		}
	}
	return email
}

// --- Users ---

// scimValidateUserFields extracts and validates the mutable user fields from
// a create/replace request body. email falls back to userName; givenName is
// required because the employee record requires a first name.
func scimValidateUserFields(w http.ResponseWriter, req scimUserRequest) (email, given, family string, ok bool) {
	userName := strings.TrimSpace(req.UserName)
	if userName == "" {
		scimWriteError(w, http.StatusBadRequest, "invalidValue", "userName is required")
		return "", "", "", false
	}
	email = scimPrimaryEmail(req)
	if !strings.Contains(email, "@") {
		scimWriteError(w, http.StatusBadRequest, "invalidValue", "userName (or emails) must be a valid email address")
		return "", "", "", false
	}
	given = strings.TrimSpace(req.Name.GivenName)
	if given == "" {
		scimWriteError(w, http.StatusBadRequest, "invalidValue", "name.givenName is required")
		return "", "", "", false
	}
	return email, given, strings.TrimSpace(req.Name.FamilyName), true
}

func (s *Server) scimListUsers(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	ls, ok := s.scimLinkStore(w, r)
	if !ok {
		return
	}
	start, count, ok := scimPagination(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var employees []store.Employee
	if raw := strings.TrimSpace(r.URL.Query().Get("filter")); raw != "" {
		attr, value, ok := parseSCIMFilter(raw)
		if !ok {
			scimWriteError(w, http.StatusBadRequest, "invalidFilter",
				`only 'userName eq "..."' and 'externalId eq "..."' filters are supported`)
			return
		}
		switch strings.ToLower(attr) {
		case "username":
			// Search is a backend hint; the exact match below is the filter.
			all, err := s.scimAllEmployees(ctx, ai.OrgID, store.EmployeeFilter{Search: value})
			if err != nil {
				scimWriteError(w, http.StatusInternalServerError, "", "internal error")
				return
			}
			for _, e := range all {
				if e.Email == value {
					employees = append(employees, e)
				}
			}
		case "externalid":
			link, err := ls.GetSCIMLinkByExternalID(ctx, ai.OrgID, value)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeSCIMList(w, []any{}, start, count)
					return
				}
				scimWriteError(w, http.StatusInternalServerError, "", "internal error")
				return
			}
			emp, err := s.store.GetEmployee(ctx, ai.OrgID, link.EmployeeID)
			if err != nil {
				scimWriteError(w, http.StatusInternalServerError, "", "internal error")
				return
			}
			employees = append(employees, emp)
		default:
			scimWriteError(w, http.StatusBadRequest, "invalidFilter",
				`only 'userName' and 'externalId' attributes can be filtered`)
			return
		}
	} else {
		var err error
		employees, err = s.scimAllEmployees(ctx, ai.OrgID, store.EmployeeFilter{})
		if err != nil {
			scimWriteError(w, http.StatusInternalServerError, "", "internal error")
			return
		}
	}
	resources := make([]any, 0, len(employees))
	for _, e := range employees {
		resources = append(resources, toSCIMUser(e, s.scimExternalID(ctx, ls, ai.OrgID, e.ID)))
	}
	writeSCIMList(w, resources, start, count)
}

func (s *Server) scimCreateUser(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	var req scimUserRequest
	if !scimDecodeJSON(w, r, &req) {
		return
	}
	email, given, family, ok := scimValidateUserFields(w, req)
	if !ok {
		return
	}
	ls, ok := s.scimLinkStore(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// Conflict pre-check before creating the employee, so a duplicate
	// externalId fails cleanly instead of leaving a half-provisioned user.
	var extID *string
	if req.ExternalID != nil && strings.TrimSpace(*req.ExternalID) != "" {
		v := strings.TrimSpace(*req.ExternalID)
		if _, err := ls.GetSCIMLinkByExternalID(ctx, ai.OrgID, v); err == nil {
			scimWriteError(w, http.StatusConflict, "uniqueness", "externalId is already linked to another user")
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			scimWriteError(w, http.StatusInternalServerError, "", "internal error")
			return
		}
		extID = &v
	}
	empNumber, ok := employeeNumberOrGenerate(w, r, nil)
	if !ok {
		return
	}
	// Provisioned users start joined (active) unless the request says
	// active:false, which is create-then-deactivate.
	emp, err := s.store.CreateEmployee(ctx, store.Employee{
		OrganizationID: ai.OrgID,
		EmployeeNumber: empNumber,
		FirstName:      given,
		LastName:       family,
		Email:          email,
		Status:         store.InvitationJoined,
	})
	if !scimMapError(w, r, err) {
		return
	}
	active := true
	if req.Active != nil {
		active = *req.Active
	}
	if !active {
		de, err := s.store.DeactivateEmployee(ctx, ai.OrgID, emp.ID, emp.Version)
		if !scimMapError(w, r, err) {
			return
		}
		emp = de
	} else {
		// New hires start as members, mirroring the /api/v1 employee create.
		if err := s.store.SetRole(ctx, ai.OrgID, emp.ID, store.RoleMember, ai.CredentialID); !scimMapError(w, r, err) {
			return
		}
	}
	if extID != nil {
		if _, err := ls.CreateSCIMLink(ctx, store.SCIMLink{
			OrganizationID: ai.OrgID,
			ExternalID:     *extID,
			EmployeeID:     emp.ID,
		}); !scimMapError(w, r, err) {
			return
		}
	}
	s.scimAudit(ctx, r, ai, "scim.user.created", "user", emp.ID, nil, emp)
	writeJSON(w, http.StatusCreated, toSCIMUser(emp, extID))
}

func (s *Server) scimGetUser(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	ls, ok := s.scimLinkStore(w, r)
	if !ok {
		return
	}
	emp, err := s.store.GetEmployee(r.Context(), ai.OrgID, r.PathValue("id"))
	if !scimMapError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, toSCIMUser(emp, s.scimExternalID(r.Context(), ls, ai.OrgID, emp.ID)))
}

// scimApplyUserUpdate is the shared write path for PUT (replace) and PATCH:
// it applies field changes via one UpdateEmployee, handles the active
// transition (deactivate/reactivate), and optionally reconciles the
// externalId link (touchLink=false leaves the link alone, for PATCH requests
// that don't mention externalId).
func (s *Server) scimApplyUserUpdate(w http.ResponseWriter, r *http.Request, ai *AuthInfo,
	cur store.Employee, email, given, family string, extID *string, touchLink bool, active *bool,
) {
	ls, ok := s.scimLinkStore(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	updated := cur
	updated.Email = email
	updated.FirstName = given
	updated.LastName = family
	if active != nil && *active && cur.Status != store.InvitationJoined {
		updated.Status = store.InvitationJoined
	}
	updated, err := s.store.UpdateEmployee(ctx, updated)
	if !scimMapError(w, r, err) {
		return
	}
	if active != nil && !*active && updated.Status == store.InvitationJoined {
		de, err := s.store.DeactivateEmployee(ctx, ai.OrgID, updated.ID, updated.Version)
		if !scimMapError(w, r, err) {
			return
		}
		updated = de
	}
	keep := s.scimExternalID(ctx, ls, ai.OrgID, updated.ID)
	if touchLink {
		var stillOK bool
		keep, stillOK = s.scimReplaceLink(w, r, ls, ai.OrgID, updated.ID, extID)
		if !stillOK {
			return
		}
	}
	s.scimAudit(ctx, r, ai, "scim.user.updated", "user", updated.ID, cur, updated)
	writeJSON(w, http.StatusOK, toSCIMUser(updated, keep))
}

func (s *Server) scimReplaceUser(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	cur, err := s.store.GetEmployee(r.Context(), ai.OrgID, r.PathValue("id"))
	if !scimMapError(w, r, err) {
		return
	}
	var req scimUserRequest
	if !scimDecodeJSON(w, r, &req) {
		return
	}
	email, given, family, ok := scimValidateUserFields(w, req)
	if !ok {
		return
	}
	// PUT is a full replacement: an absent externalId drops the existing
	// link; an absent active flag keeps the current status.
	var extID *string
	if req.ExternalID != nil && strings.TrimSpace(*req.ExternalID) != "" {
		v := strings.TrimSpace(*req.ExternalID)
		extID = &v
	}
	s.scimApplyUserUpdate(w, r, ai, cur, email, given, family, extID, true, req.Active)
}

// scimReplaceLink reconciles the employee's externalId link to want (nil =
// no link). It writes the error response on failure.
func (s *Server) scimReplaceLink(w http.ResponseWriter, r *http.Request, ls store.SCIMLinkStore,
	orgID, employeeID string, want *string,
) (*string, bool) {
	ctx := r.Context()
	cur, err := ls.GetSCIMLinkByEmployee(ctx, orgID, employeeID)
	have := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		scimWriteError(w, http.StatusInternalServerError, "", "internal error")
		return nil, false
	}
	if have && (want == nil || cur.ExternalID != *want) {
		if err := ls.DeleteSCIMLinkByEmployee(ctx, orgID, employeeID); err != nil {
			scimWriteError(w, http.StatusInternalServerError, "", "internal error")
			return nil, false
		}
		have = false
	}
	if want != nil && !have {
		if other, err := ls.GetSCIMLinkByExternalID(ctx, orgID, *want); err == nil && other.EmployeeID != employeeID {
			scimWriteError(w, http.StatusConflict, "uniqueness", "externalId is already linked to another user")
			return nil, false
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			scimWriteError(w, http.StatusInternalServerError, "", "internal error")
			return nil, false
		}
		if _, err := ls.CreateSCIMLink(ctx, store.SCIMLink{
			OrganizationID: orgID,
			ExternalID:     *want,
			EmployeeID:     employeeID,
		}); !scimMapError(w, r, err) {
			return nil, false
		}
	}
	return want, true
}

func (s *Server) scimPatchUser(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	cur, err := s.store.GetEmployee(r.Context(), ai.OrgID, r.PathValue("id"))
	if !scimMapError(w, r, err) {
		return
	}
	var req scimPatchRequest
	if !scimDecodeJSON(w, r, &req) {
		return
	}
	ops := req.ops()
	if len(ops) == 0 {
		scimWriteError(w, http.StatusBadRequest, "invalidValue", "PATCH requires at least one operation")
		return
	}
	email, given, family := cur.Email, cur.FirstName, cur.LastName
	var active *bool
	var extID *string
	extSet := false // tri-state: extSet=false means "leave the link alone"
	for _, op := range ops {
		o := strings.ToLower(strings.TrimSpace(op.Op))
		if o != "replace" && o != "add" && o != "remove" {
			scimWriteError(w, http.StatusBadRequest, "invalidValue", "unsupported op "+strconv.Quote(op.Op)+": only replace, add, and remove are supported")
			return
		}
		path := strings.ToLower(strings.TrimSpace(op.Path))
		if o == "remove" && path != "externalid" {
			scimWriteError(w, http.StatusBadRequest, "invalidPath", "remove is only supported on path externalId")
			return
		}
		if o == "remove" {
			extID, extSet = nil, true
			continue
		}
		switch path {
		case "active":
			var b bool
			if err := json.Unmarshal(op.Value, &b); err != nil {
				scimWriteError(w, http.StatusBadRequest, "invalidValue", "active must be a boolean")
				return
			}
			active = &b
		case "name.givenname":
			var v string
			if err := json.Unmarshal(op.Value, &v); err != nil || strings.TrimSpace(v) == "" {
				scimWriteError(w, http.StatusBadRequest, "invalidValue", "name.givenName must be a non-empty string")
				return
			}
			given = strings.TrimSpace(v)
		case "name.familyname":
			var v string
			if err := json.Unmarshal(op.Value, &v); err != nil {
				scimWriteError(w, http.StatusBadRequest, "invalidValue", "name.familyName must be a string")
				return
			}
			family = strings.TrimSpace(v)
		case "emails":
			v, ok := scimParseEmails(op.Value)
			if !ok || !strings.Contains(v, "@") {
				scimWriteError(w, http.StatusBadRequest, "invalidValue", "emails must contain a valid email address")
				return
			}
			email = v
		case "externalid":
			var v string
			if err := json.Unmarshal(op.Value, &v); err != nil || strings.TrimSpace(v) == "" {
				scimWriteError(w, http.StatusBadRequest, "invalidValue", "externalId must be a non-empty string")
				return
			}
			v = strings.TrimSpace(v)
			extID, extSet = &v, true
		default:
			scimWriteError(w, http.StatusBadRequest, "invalidPath", "unsupported path "+strconv.Quote(op.Path))
			return
		}
	}
	var linkWant **string // nil = leave alone; *nil = drop; **v = set
	if extSet {
		linkWant = &extID
	}
	// touchLink=false leaves the link alone (PATCH without externalId);
	// touchLink=true reconciles it (PUT, or PATCH mentioning externalId).
	touchLink := linkWant != nil
	var want *string
	if touchLink {
		want = *linkWant
	}
	s.scimApplyUserUpdate(w, r, ai, cur, email, given, family, want, touchLink, active)
}

// scimParseEmails extracts one email from an emails PATCH value, accepting
// [{"value":...}], {"value":...}, or a bare string.
func scimParseEmails(raw json.RawMessage) (string, bool) {
	var arr []scimEmail
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		v := strings.TrimSpace(arr[0].Value)
		for _, e := range arr {
			if e.Primary && strings.TrimSpace(e.Value) != "" {
				v = strings.TrimSpace(e.Value)
				break
			}
		}
		if v != "" {
			return v, true
		}
	}
	var single scimEmail
	if err := json.Unmarshal(raw, &single); err == nil && strings.TrimSpace(single.Value) != "" {
		return strings.TrimSpace(single.Value), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s), true
	}
	return "", false
}

func (s *Server) scimDeleteUser(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	ctx := r.Context()
	before, err := s.store.GetEmployee(ctx, ai.OrgID, r.PathValue("id"))
	if !scimMapError(w, r, err) {
		return
	}
	// Soft-deactivate only: the record and its externalId link stay, so the
	// IdP can re-correlate on re-provision. Already-removed is a no-op 204.
	updated := before
	if before.Status != store.InvitationRemoved {
		updated, err = s.store.DeactivateEmployee(ctx, ai.OrgID, before.ID, before.Version)
		if !scimMapError(w, r, err) {
			return
		}
	}
	s.scimAudit(ctx, r, ai, "scim.user.deactivated", "user", updated.ID, before, updated)
	w.WriteHeader(http.StatusNoContent)
}

// --- Groups ---

// scimGroupMembers returns the SCIM member entries for a group: every
// employee whose group_id points at it.
func (s *Server) scimGroupMembers(ctx context.Context, orgID, groupID string) ([]scimGroupMember, error) {
	employees, err := s.scimAllEmployees(ctx, orgID, store.EmployeeFilter{GroupID: groupID})
	if err != nil {
		return nil, err
	}
	members := make([]scimGroupMember, 0, len(employees))
	for _, e := range employees {
		members = append(members, scimGroupMember{
			Value:   e.ID,
			Ref:     "Users/" + e.ID,
			Display: strings.TrimSpace(e.FirstName + " " + e.LastName),
		})
	}
	return members, nil
}

func toSCIMGroup(g store.Group, members []scimGroupMember) scimGroup {
	if members == nil {
		members = []scimGroupMember{}
	}
	return scimGroup{
		Schemas:     []string{scimSchemaGroup},
		ID:          g.ID,
		DisplayName: g.Name,
		Members:     members,
		Meta: scimMeta{
			ResourceType: "Group",
			Created:      g.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			LastModified: g.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		},
	}
}

func (s *Server) scimListGroups(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	start, count, ok := scimPagination(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	groups, err := s.scimAllGroups(ctx, ai.OrgID)
	if err != nil {
		scimWriteError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("filter")); raw != "" {
		attr, value, ok := parseSCIMFilter(raw)
		if !ok || !strings.EqualFold(attr, "displayName") {
			scimWriteError(w, http.StatusBadRequest, "invalidFilter",
				`only 'displayName eq "..."' filters are supported on Groups`)
			return
		}
		var filtered []store.Group
		for _, g := range groups {
			if g.Name == value {
				filtered = append(filtered, g)
			}
		}
		groups = filtered
	}
	resources := make([]any, 0, len(groups))
	for _, g := range groups {
		members, err := s.scimGroupMembers(ctx, ai.OrgID, g.ID)
		if err != nil {
			scimWriteError(w, http.StatusInternalServerError, "", "internal error")
			return
		}
		resources = append(resources, toSCIMGroup(g, members))
	}
	writeSCIMList(w, resources, start, count)
}

// scimAssignGroupMembers sets the group_id of each listed employee to the
// group (add=true) or clears it (add=false, only when it currently points at
// the group). All employee IDs are validated first so a bad member fails the
// whole request instead of half-applying it.
func (s *Server) scimAssignGroupMembers(w http.ResponseWriter, r *http.Request, ai *AuthInfo,
	groupID string, memberIDs []string, add bool,
) bool {
	ctx := r.Context()
	// Phase 1: validate.
	emps := make([]store.Employee, 0, len(memberIDs))
	for _, id := range memberIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		emp, err := s.store.GetEmployee(ctx, ai.OrgID, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				scimWriteError(w, http.StatusBadRequest, "invalidValue", "unknown member "+strconv.Quote(id))
			} else {
				scimWriteError(w, http.StatusInternalServerError, "", "internal error")
			}
			return false
		}
		emps = append(emps, emp)
	}
	// Phase 2: apply.
	for _, emp := range emps {
		if add {
			if emp.GroupID != nil && *emp.GroupID == groupID {
				continue
			}
			emp.GroupID = &groupID
		} else {
			if emp.GroupID == nil || *emp.GroupID != groupID {
				continue
			}
			emp.GroupID = nil
		}
		if _, err := s.store.UpdateEmployee(ctx, emp); !scimMapError(w, r, err) {
			return false
		}
	}
	return true
}

func (s *Server) scimCreateGroup(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	var req scimGroupRequest
	if !scimDecodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		scimWriteError(w, http.StatusBadRequest, "invalidValue", "displayName is required")
		return
	}
	if req.ExternalID != nil && strings.TrimSpace(*req.ExternalID) != "" {
		// The scim_links table maps externalIds to employees only; there is
		// nowhere to persist a group externalId, so fail loudly rather than
		// silently drop the IdP's correlation key.
		scimWriteError(w, http.StatusBadRequest, "invalidValue",
			"group externalId is not supported in this release")
		return
	}
	ctx := r.Context()
	g, err := s.store.CreateGroup(ctx, store.Group{
		OrganizationID: ai.OrgID,
		Name:           strings.TrimSpace(req.DisplayName),
	})
	if !scimMapError(w, r, err) {
		return
	}
	var memberIDs []string
	for _, m := range req.Members {
		memberIDs = append(memberIDs, m.Value)
	}
	if !s.scimAssignGroupMembers(w, r, ai, g.ID, memberIDs, true) {
		return
	}
	members, err := s.scimGroupMembers(ctx, ai.OrgID, g.ID)
	if err != nil {
		scimWriteError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	s.scimAudit(ctx, r, ai, "scim.group.created", "group", g.ID, nil, g)
	writeJSON(w, http.StatusCreated, toSCIMGroup(g, members))
}

func (s *Server) scimGetGroup(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	g, err := s.store.GetGroup(r.Context(), ai.OrgID, r.PathValue("id"))
	if !scimMapError(w, r, err) {
		return
	}
	members, err := s.scimGroupMembers(r.Context(), ai.OrgID, g.ID)
	if err != nil {
		scimWriteError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toSCIMGroup(g, members))
}

// scimParseMemberIDs extracts employee IDs from a members PATCH value,
// accepting [{value:...}], {value:...}, or a bare string ID.
func scimParseMemberIDs(raw json.RawMessage) []string {
	var arr []struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &arr); err == nil {
		var ids []string
		for _, m := range arr {
			if strings.TrimSpace(m.Value) != "" {
				ids = append(ids, strings.TrimSpace(m.Value))
			}
		}
		return ids
	}
	var single struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && strings.TrimSpace(single.Value) != "" {
		return []string{strings.TrimSpace(single.Value)}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
		return []string{strings.TrimSpace(s)}
	}
	return nil
}

func (s *Server) scimPatchGroup(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	ctx := r.Context()
	cur, err := s.store.GetGroup(ctx, ai.OrgID, r.PathValue("id"))
	if !scimMapError(w, r, err) {
		return
	}
	var req scimPatchRequest
	if !scimDecodeJSON(w, r, &req) {
		return
	}
	ops := req.ops()
	if len(ops) == 0 {
		scimWriteError(w, http.StatusBadRequest, "invalidValue", "PATCH requires at least one operation")
		return
	}
	updated := cur
	for _, op := range ops {
		o := strings.ToLower(strings.TrimSpace(op.Op))
		path := strings.ToLower(strings.TrimSpace(op.Path))
		switch {
		case path == "displayname" && (o == "replace" || o == "add"):
			var v string
			if err := json.Unmarshal(op.Value, &v); err != nil || strings.TrimSpace(v) == "" {
				scimWriteError(w, http.StatusBadRequest, "invalidValue", "displayName must be a non-empty string")
				return
			}
			updated.Name = strings.TrimSpace(v)
		case (path == "members" || path == "members.value") && (o == "add" || o == "remove"):
			ids := scimParseMemberIDs(op.Value)
			if len(ids) == 0 {
				scimWriteError(w, http.StatusBadRequest, "invalidValue", "members value must list at least one member id")
				return
			}
			if !s.scimAssignGroupMembers(w, r, ai, cur.ID, ids, o == "add") {
				return
			}
		default:
			scimWriteError(w, http.StatusBadRequest, "invalidPath",
				"unsupported path "+strconv.Quote(op.Path)+": only displayName and members are supported")
			return
		}
	}
	if updated.Name != cur.Name {
		updated, err = s.store.UpdateGroup(ctx, updated)
		if !scimMapError(w, r, err) {
			return
		}
	}
	members, err := s.scimGroupMembers(ctx, ai.OrgID, updated.ID)
	if err != nil {
		scimWriteError(w, http.StatusInternalServerError, "", "internal error")
		return
	}
	s.scimAudit(ctx, r, ai, "scim.group.updated", "group", updated.ID, cur, updated)
	writeJSON(w, http.StatusOK, toSCIMGroup(updated, members))
}

func (s *Server) scimDeleteGroup(w http.ResponseWriter, r *http.Request, ai *AuthInfo) {
	before, err := s.store.GetGroup(r.Context(), ai.OrgID, r.PathValue("id"))
	if !scimMapError(w, r, err) {
		return
	}
	// The store refuses to delete a group that still has members assigned
	// (ErrNotEmpty -> 409); the IdP must unassign members first.
	if err := s.store.DeleteGroup(r.Context(), ai.OrgID, before.ID); !scimMapError(w, r, err) {
		return
	}
	s.scimAudit(r.Context(), r, ai, "scim.group.deleted", "group", before.ID, before, nil)
	w.WriteHeader(http.StatusNoContent)
}
