package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors returned by Store implementations. Use errors.Is to match.
var (
	// ErrNotFound means no such organization, group, employee, membership,
	// credential, audit event, or idempotency record.
	ErrNotFound = errors.New("store: not found")
	// ErrAlreadyExists means the slug, employee number, email, token hash,
	// or id is already taken.
	ErrAlreadyExists = errors.New("store: already exists")
	// ErrVersionConflict means the record changed since it was read.
	// Callers should re-read and retry.
	ErrVersionConflict = errors.New("store: version conflict")
	// ErrNotEmpty means a group still has members or child groups and
	// cannot be deleted.
	ErrNotEmpty = errors.New("store: group not empty")
	// ErrCredentialInvalid means the credential is revoked or expired.
	// Unknown token hashes return ErrNotFound.
	ErrCredentialInvalid = errors.New("store: credential invalid")
)

// Pagination bounds. Agents page aggressively; the store caps pages so one
// caller can't force a full-table scan into memory.
const (
	DefaultListLimit = 100
	MaxListLimit     = 1000
)

// IdempotencyTTL is how long idempotency records stay valid. Readers treat
// older records as absent.
const IdempotencyTTL = 24 * time.Hour

// Page selects one page of a cursor-paginated listing. Limit <= 0 means
// DefaultListLimit (capped at MaxListLimit). Cursor is an opaque token from
// a previous listing's next cursor; empty starts at the beginning.
type Page struct {
	Limit  int
	Cursor string
}

// normalizeLimit applies the default and the cap.
func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultListLimit
	}
	if limit > MaxListLimit {
		return MaxListLimit
	}
	return limit
}

// encodeCursor builds an opaque cursor from (created_at, id). Listings are
// ordered by (created_at, id), so the cursor marks a resume position.
func encodeCursor(t time.Time, id string) string {
	raw := t.UTC().Format(time.RFC3339Nano) + "|" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses a cursor back into (created_at, id).
func decodeCursor(cursor string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("store: invalid cursor: %w", err)
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return time.Time{}, "", errors.New("store: invalid cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("store: invalid cursor: %w", err)
	}
	return t, parts[1], nil
}

// afterCursor reports whether (t, id) sorts strictly after (ct, cid) under
// the (created_at, id) ordering used by every listing.
func afterCursor(t time.Time, id string, ct time.Time, cid string) bool {
	if t.After(ct) {
		return true
	}
	return t.Equal(ct) && id > cid
}

// auditCanonical is the deterministic body the audit hash chain commits to.
// Struct field order + encoding/json's deterministic marshaling make the
// canonical form stable across implementations.
type auditCanonical struct {
	OrganizationID string          `json:"organization_id"`
	ActorType      ActorType       `json:"actor_type"`
	ActorID        string          `json:"actor_id"`
	Action         string          `json:"action"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id,omitempty"`
	Before         json.RawMessage `json:"before,omitempty"`
	After          json.RawMessage `json:"after,omitempty"`
	CreatedAt      string          `json:"created_at"` // RFC3339Nano, UTC
}

// computeAuditHash returns the hash for an audit event:
// hex(sha256(prevHash + "|" + canonicalJSON(body))). prevHash is "" for the
// first event of an organization.
func computeAuditHash(prevHash string, e AuditEvent) (string, error) {
	canon := auditCanonical{
		OrganizationID: e.OrganizationID,
		ActorType:      e.ActorType,
		ActorID:        e.ActorID,
		Action:         e.Action,
		ResourceType:   e.ResourceType,
		ResourceID:     e.ResourceID,
		Before:         e.Before,
		After:          e.After,
		CreatedAt:      e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(canon)
	if err != nil {
		return "", fmt.Errorf("store: canonicalize audit event: %w", err)
	}
	sum := sha256.Sum256([]byte(prevHash + "|" + string(b)))
	return hex.EncodeToString(sum[:]), nil
}

// credentialHash returns the stored token digest: hex(sha256(plaintext)).
func credentialHash(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// EmployeeFilter selects employees for listing. Zero values mean "any".
type EmployeeFilter struct {
	GroupID string
	Status  InvitationStatus
	Search  string // matches first/last name, email, employee number
}

// AuditFilter selects audit events for listing. Zero values mean "any".
type AuditFilter struct {
	ActorType    ActorType
	Action       string
	ResourceType string
	Since        *time.Time
}

// Store is the core-platform contract (component 1: identity, tenancy,
// groups, RBAC credentials, hash-chained audit log, idempotency).
//
// Implementations must be safe for concurrent use and must deep-copy values
// across the boundary, so a caller can never mutate stored state through a
// previously returned value.
type Store interface {
	// Organizations.
	// CreateOrganization stores a new organization. An empty ID is
	// generated; slug must be globally unique. Version starts at 1.
	// Returns ErrAlreadyExists for a taken slug or ID.
	CreateOrganization(ctx context.Context, o Organization) (Organization, error)
	// GetOrganization returns the organization or ErrNotFound.
	GetOrganization(ctx context.Context, id string) (Organization, error)
	// GetOrganizationBySlug returns the organization or ErrNotFound.
	GetOrganizationBySlug(ctx context.Context, slug string) (Organization, error)
	// UpdateOrganization applies o as the new state iff o.Version matches
	// the stored version (optimistic concurrency). On success the returned
	// organization carries Version+1; on mismatch it returns
	// ErrVersionConflict (ErrNotFound when the ID is unknown).
	UpdateOrganization(ctx context.Context, o Organization) (Organization, error)
	// ListOrganizations returns organizations ordered by (created_at, id),
	// paged; next is the cursor for the following page ("" when done).
	ListOrganizations(ctx context.Context, p Page) (orgs []Organization, next string, err error)

	// Groups (departments/teams).
	// CreateGroup stores a new group in an existing organization; the
	// parent, when set, must be a group in the same organization.
	// Returns ErrNotFound for an unknown organization or parent.
	CreateGroup(ctx context.Context, g Group) (Group, error)
	// GetGroup returns the group or ErrNotFound.
	GetGroup(ctx context.Context, orgID, id string) (Group, error)
	// UpdateGroup applies g as the new state iff g.Version matches.
	// Returns ErrVersionConflict on mismatch, ErrNotFound when unknown.
	UpdateGroup(ctx context.Context, g Group) (Group, error)
	// DeleteGroup removes an empty group. Returns ErrNotEmpty when the
	// group still has members or child groups.
	DeleteGroup(ctx context.Context, orgID, id string) error
	// ListGroups returns the organization's groups ordered by
	// (created_at, id), paged.
	ListGroups(ctx context.Context, orgID string, p Page) (groups []Group, next string, err error)

	// Employees.
	// CreateEmployee stores a new employee. employee_number must be unique
	// per organization; email must be unique per organization when
	// non-empty. Returns ErrAlreadyExists on conflict, ErrNotFound for an
	// unknown organization, group, or manager.
	CreateEmployee(ctx context.Context, e Employee) (Employee, error)
	// GetEmployee returns the employee or ErrNotFound.
	GetEmployee(ctx context.Context, orgID, id string) (Employee, error)
	// UpdateEmployee applies e as the new state iff e.Version matches.
	// Returns ErrVersionConflict on mismatch, ErrNotFound when unknown.
	UpdateEmployee(ctx context.Context, e Employee) (Employee, error)
	// DeactivateEmployee sets Status to removed iff version matches, and
	// returns the deactivated employee. There are no hard deletes of
	// employees; the record stays for audit.
	DeactivateEmployee(ctx context.Context, orgID, id string, version int64) (Employee, error)
	// ListEmployees returns matching employees ordered by (created_at, id),
	// paged.
	ListEmployees(ctx context.Context, orgID string, f EmployeeFilter, p Page) (employees []Employee, next string, err error)

	// Memberships (one role per employee per organization).
	// SetRole upserts the employee's role. The employee must exist and have
	// Status joined; grantedBy may be empty. Returns ErrNotFound for an
	// unknown employee.
	SetRole(ctx context.Context, orgID, employeeID string, role Role, grantedBy string) error
	// GetRole returns the employee's role or ErrNotFound when the employee
	// has no membership.
	GetRole(ctx context.Context, orgID, employeeID string) (Role, error)
	// ListMemberships returns the organization's memberships ordered by
	// (created_at, employee_id), paged.
	ListMemberships(ctx context.Context, orgID string, p Page) (memberships []Membership, next string, err error)

	// Credentials.
	// IssueCredential stores a new credential, hashing tokenPlaintext with
	// SHA-256 into TokenHash. The plaintext is never stored. Returns the
	// stored credential (TokenHash populated). ErrNotFound for an unknown
	// organization or PAT employee; ErrAlreadyExists for a duplicate token
	// hash or ID.
	IssueCredential(ctx context.Context, c Credential, tokenPlaintext string) (Credential, error)
	// GetCredentialByTokenHash looks a credential up by its token digest.
	// Returns ErrNotFound for an unknown hash, ErrCredentialInvalid when
	// the credential is revoked or expired.
	GetCredentialByTokenHash(ctx context.Context, hash string) (Credential, error)
	// GetCredential returns the credential by ID within the organization, or
	// ErrNotFound. Unlike GetCredentialByTokenHash it does not reject
	// revoked or expired credentials — it is the administration read path
	// (revocation, audit trails).
	GetCredential(ctx context.Context, orgID, id string) (Credential, error)
	// RevokeCredential sets RevokedAt. Idempotent; ErrNotFound when unknown.
	RevokeCredential(ctx context.Context, orgID, id string) error
	// TouchCredential records last-use time. ErrNotFound when unknown.
	TouchCredential(ctx context.Context, orgID, id string, at time.Time) error
	// ListCredentials returns the organization's credentials ordered by
	// (created_at, id), paged. TokenHash is never serialized (json:"-").
	ListCredentials(ctx context.Context, orgID string, p Page) (creds []Credential, next string, err error)

	// Audit log (append-only, hash-chained).
	// AppendAuditEvent appends one event. The store assigns ID and
	// CreatedAt, links PrevHash to the organization's previous event ("" for
	// the first), and computes Hash. Returns the stored event.
	AppendAuditEvent(ctx context.Context, e AuditEvent) (AuditEvent, error)
	// ListAuditEvents returns matching events ordered by (created_at, id),
	// paged.
	ListAuditEvents(ctx context.Context, orgID string, f AuditFilter, p Page) (events []AuditEvent, next string, err error)

	// Idempotency.
	// IdempotencyGet returns the cached record, or found=false when the key
	// is unknown. Records older than IdempotencyTTL (24h) are treated as
	// absent.
	IdempotencyGet(ctx context.Context, orgID, key string) (rec IdempotencyRecord, found bool, err error)
	// IdempotencyPut upserts a record (same key+org replaces the previous
	// one, refreshing its timestamp).
	IdempotencyPut(ctx context.Context, r IdempotencyRecord) error
}
