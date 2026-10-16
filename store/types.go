// Package store implements forge-hris component 1: the core platform store
// (identity, tenancy, groups, RBAC credentials, hash-chained audit log, and
// idempotency records).
//
// Design notes for high load (massive concurrent AI-agent workload):
//   - Every method takes a context.Context so callers can cancel and bound
//     latency; adapters are expected to be backed by a connection pool.
//   - Writes use optimistic concurrency: Organization, Group, Employee and
//     Credential carry a Version that must match on update, otherwise
//     ErrVersionConflict.
//   - The audit log is an append-only hash chain per organization; the hash
//     of each event commits to its predecessor, so tampering is detectable.
//   - Tenancy is enforced in Postgres with row-level security
//     (see migrations/001_init.sql): every org-scoped operation runs inside
//     a transaction with app.tenant_id set, and queries also carry explicit
//     organization_id predicates (belt and suspenders).
//   - Implementations must deep-copy values across the store boundary, so a
//     caller can never mutate stored state via a previously returned value.
package store

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// Role is the single role an employee holds within an organization.
type Role string

const (
	RoleOwner   Role = "owner"
	RoleAdmin   Role = "admin"
	RoleManager Role = "manager"
	RoleMember  Role = "member"
)

// ValidRole reports whether r is a known role.
func ValidRole(r Role) bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleManager, RoleMember:
		return true
	}
	return false
}

// InvitationStatus tracks an employee's membership lifecycle.
type InvitationStatus string

const (
	InvitationNotInvited InvitationStatus = "not_invited"
	InvitationPending    InvitationStatus = "pending_invitation"
	InvitationJoined     InvitationStatus = "joined"
	InvitationRemoved    InvitationStatus = "removed"
)

// ValidInvitationStatus reports whether s is a known invitation status.
func ValidInvitationStatus(s InvitationStatus) bool {
	switch s {
	case InvitationNotInvited, InvitationPending, InvitationJoined, InvitationRemoved:
		return true
	}
	return false
}

// CredentialKind distinguishes a personal access token (bound to one
// employee, inherits that employee's role) from a service account (bound to
// an explicit scope list, no employee).
type CredentialKind string

const (
	CredentialPAT            CredentialKind = "pat"
	CredentialServiceAccount CredentialKind = "service_account"
)

// ValidCredentialKind reports whether k is a known credential kind.
func ValidCredentialKind(k CredentialKind) bool {
	switch k {
	case CredentialPAT, CredentialServiceAccount:
		return true
	}
	return false
}

// ActorType identifies who performed an audited action. Human principals,
// agent principals (Component 2/MCP), automated system work, and SCIM
// provisioning are all first-class actors.
type ActorType string

const (
	ActorHuman  ActorType = "human"
	ActorAgent  ActorType = "agent"
	ActorSystem ActorType = "system"
	ActorSCIM   ActorType = "scim"
)

// ValidActorType reports whether a is a known actor type.
func ValidActorType(a ActorType) bool {
	switch a {
	case ActorHuman, ActorAgent, ActorSystem, ActorSCIM:
		return true
	}
	return false
}

// EmploymentType is the employee's engagement classification.
type EmploymentType string

const (
	EmploymentFullTime EmploymentType = "full_time"
	EmploymentPartTime EmploymentType = "part_time"
	EmploymentContract EmploymentType = "contract"
	EmploymentIntern   EmploymentType = "intern"
)

// ValidEmploymentType reports whether e is a known employment type.
func ValidEmploymentType(e EmploymentType) bool {
	switch e {
	case EmploymentFullTime, EmploymentPartTime, EmploymentContract, EmploymentIntern:
		return true
	}
	return false
}

// Scope constants for credential scoping (service accounts; PATs inherit
// their owner's role). Components 2-6 check these at the API boundary.
const (
	ScopeTimeRead       = "time:read"
	ScopeTimeWrite      = "time:write"
	ScopeLeaveRead      = "leave:read"
	ScopeLeaveWrite     = "leave:write"
	ScopeLeaveApprove   = "leave:approve"
	ScopePayrollRead    = "payroll:read"
	ScopePayrollPreview = "payroll:preview"
	ScopePayrollSubmit  = "payroll:submit"
	ScopeEmployeesRead  = "employees:read"
	ScopeEmployeesWrite = "employees:write"
	ScopeAuditRead      = "audit:read"
)

// AllScopes returns every known scope, in canonical order.
func AllScopes() []string {
	return []string{
		ScopeTimeRead, ScopeTimeWrite,
		ScopeLeaveRead, ScopeLeaveWrite, ScopeLeaveApprove,
		ScopePayrollRead, ScopePayrollPreview, ScopePayrollSubmit,
		ScopeEmployeesRead, ScopeEmployeesWrite, ScopeAuditRead,
	}
}

// ValidScope reports whether s is a known scope.
func ValidScope(s string) bool {
	for _, known := range AllScopes() {
		if s == known {
			return true
		}
	}
	return false
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// Organization is a tenant: one company (MY or ID statutory scope).
type Organization struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	CountryCode string    `json:"country_code"` // MY | ID
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Version     int64     `json:"version"` // optimistic concurrency token
}

// Validate checks the organization's shape.
func (o Organization) Validate() error {
	if strings.TrimSpace(o.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	if strings.TrimSpace(o.Slug) == "" {
		return &ValidationError{Field: "slug", Message: "required"}
	}
	if !slugPattern.MatchString(o.Slug) {
		return &ValidationError{Field: "slug", Message: "must be lowercase alphanumeric with hyphens"}
	}
	if o.CountryCode != "MY" && o.CountryCode != "ID" {
		return &ValidationError{Field: "country_code", Message: "must be MY or ID"}
	}
	return nil
}

// Group is a department/team. ParentID builds the org-chart hierarchy;
// nil means a top-level group.
type Group struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	Name           string    `json:"name"`
	ParentID       *string   `json:"parent_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Version        int64     `json:"version"` // optimistic concurrency token
}

// Validate checks the group's shape.
func (g Group) Validate() error {
	if strings.TrimSpace(g.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(g.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	return nil
}

// Employee is a person record. Status tracks the invitation lifecycle;
// deactivation sets Status to removed (no hard deletes).
type Employee struct {
	ID             string           `json:"id"`
	OrganizationID string           `json:"organization_id"`
	EmployeeNumber string           `json:"employee_number"` // unique per organization
	FirstName      string           `json:"first_name"`
	LastName       string           `json:"last_name,omitempty"`
	Email          string           `json:"email,omitempty"` // unique per org when non-empty
	Position       string           `json:"position,omitempty"`
	EmploymentType EmploymentType   `json:"employment_type"`
	GroupID        *string          `json:"group_id,omitempty"`
	ManagerID      *string          `json:"manager_id,omitempty"` // self reference
	TimeZone       string           `json:"time_zone,omitempty"`  // IANA name
	WorkStartDate  *time.Time       `json:"work_start_date,omitempty"`
	Status         InvitationStatus `json:"status"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Version        int64            `json:"version"` // optimistic concurrency token
}

// Validate checks the employee's shape.
func (e Employee) Validate() error {
	if strings.TrimSpace(e.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(e.EmployeeNumber) == "" {
		return &ValidationError{Field: "employee_number", Message: "required"}
	}
	if strings.TrimSpace(e.FirstName) == "" {
		return &ValidationError{Field: "first_name", Message: "required"}
	}
	if e.Email != "" && !strings.Contains(e.Email, "@") {
		return &ValidationError{Field: "email", Message: "must be a valid email address"}
	}
	if e.EmploymentType != "" && !ValidEmploymentType(e.EmploymentType) {
		return &ValidationError{Field: "employment_type", Message: "unknown employment type"}
	}
	if e.Status != "" && !ValidInvitationStatus(e.Status) {
		return &ValidationError{Field: "status", Message: "unknown invitation status"}
	}
	return nil
}

// Membership binds one employee to one role within an organization.
type Membership struct {
	OrganizationID string    `json:"organization_id"`
	EmployeeID     string    `json:"employee_id"`
	Role           Role      `json:"role"`
	GrantedBy      *string   `json:"granted_by,omitempty"` // actor ID that granted the role
	CreatedAt      time.Time `json:"created_at"`
}

// Validate checks the membership's shape.
func (m Membership) Validate() error {
	if strings.TrimSpace(m.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(m.EmployeeID) == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if !ValidRole(m.Role) {
		return &ValidationError{Field: "role", Message: "unknown role"}
	}
	return nil
}

// Credential is an API credential: a personal access token (PAT, bound to an
// employee and inheriting their role) or a service account (explicit scopes,
// no employee). The raw token is never stored; only its SHA-256 hex digest.
type Credential struct {
	ID             string         `json:"id"`
	OrganizationID string         `json:"organization_id"`
	Kind           CredentialKind `json:"kind"`
	Name           string         `json:"name"`
	TokenHash      string         `json:"-"`                     // sha256 hex of the plaintext token; never serialized
	EmployeeID     *string        `json:"employee_id,omitempty"` // PAT owner; nil for service accounts
	Scopes         []string       `json:"scopes,omitempty"`      // service accounts; PATs inherit role
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	RevokedAt      *time.Time     `json:"revoked_at,omitempty"`
	LastUsedAt     *time.Time     `json:"last_used_at,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	Version        int64          `json:"version"` // optimistic concurrency token
}

// Validate checks the credential's shape.
func (c Credential) Validate() error {
	if strings.TrimSpace(c.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if !ValidCredentialKind(c.Kind) {
		return &ValidationError{Field: "kind", Message: "unknown credential kind"}
	}
	if strings.TrimSpace(c.Name) == "" {
		return &ValidationError{Field: "name", Message: "required"}
	}
	switch c.Kind {
	case CredentialPAT:
		if c.EmployeeID == nil || strings.TrimSpace(*c.EmployeeID) == "" {
			return &ValidationError{Field: "employee_id", Message: "required for pat credentials"}
		}
	case CredentialServiceAccount:
		if c.EmployeeID != nil {
			return &ValidationError{Field: "employee_id", Message: "must be nil for service accounts"}
		}
		if len(c.Scopes) == 0 {
			return &ValidationError{Field: "scopes", Message: "at least one scope is required for service accounts"}
		}
	}
	for _, s := range c.Scopes {
		if !ValidScope(s) {
			return &ValidationError{Field: "scopes", Message: "unknown scope: " + s}
		}
	}
	return nil
}

// AuditEvent is one entry in the append-only, hash-chained audit log. Hash
// commits to PrevHash plus the canonical event body, so any tampering with
// history breaks the chain.
type AuditEvent struct {
	ID             int64           `json:"id"`
	OrganizationID string          `json:"organization_id"`
	ActorType      ActorType       `json:"actor_type"`
	ActorID        string          `json:"actor_id"`
	OnBehalfOf     *string         `json:"on_behalf_of,omitempty"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id,omitempty"`
	Before         json.RawMessage `json:"before,omitempty"`
	After          json.RawMessage `json:"after,omitempty"`
	IdempotencyKey *string         `json:"idempotency_key,omitempty"`
	PrevHash       string          `json:"prev_hash"`
	Hash           string          `json:"hash"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Validate checks the audit event's shape (the store assigns ID, CreatedAt
// and the hash chain fields).
func (a AuditEvent) Validate() error {
	if strings.TrimSpace(a.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if !ValidActorType(a.ActorType) {
		return &ValidationError{Field: "actor_type", Message: "unknown actor type"}
	}
	if strings.TrimSpace(a.ActorID) == "" {
		return &ValidationError{Field: "actor_id", Message: "required"}
	}
	if strings.TrimSpace(a.Action) == "" {
		return &ValidationError{Field: "action", Message: "required"}
	}
	if strings.TrimSpace(a.ResourceType) == "" {
		return &ValidationError{Field: "resource_type", Message: "required"}
	}
	return nil
}

// IdempotencyRecord caches one request's response so retries with the same
// key replay the original result instead of executing twice. Readers treat
// records older than 24h as absent.
type IdempotencyRecord struct {
	OrganizationID string    `json:"organization_id"`
	Key            string    `json:"key"`
	Fingerprint    string    `json:"fingerprint,omitempty"` // hash of the request payload
	StatusCode     int       `json:"status_code"`
	ResponseBody   []byte    `json:"response_body,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Validate checks the idempotency record's shape.
func (r IdempotencyRecord) Validate() error {
	if strings.TrimSpace(r.OrganizationID) == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if strings.TrimSpace(r.Key) == "" {
		return &ValidationError{Field: "key", Message: "required"}
	}
	return nil
}

// ValidationError is returned when a value fails Validate.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }
