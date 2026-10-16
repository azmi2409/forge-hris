// Package store — Xero integration (component 6, Worker A).
//
// OAuth connections to Xero (tokens stored encrypted via the
// integrations/secrets envelope — the store only ever sees the sealed
// blobs), per-org account/contact mappings, and a posting ledger so a
// payroll run's bills are posted at most once per employee.
//
// XeroStore is intentionally narrow: the integrations/xero package defines
// its own structural Store interface over these same methods, so store.go
// stays untouched and both adapters satisfy it without an import cycle.
package store

import (
	"context"
	"time"
)

// XeroConnection is one org's OAuth connection to Xero. Tokens are
// AES-256-GCM envelopes (see integrations/secrets); they are never stored
// or logged in plaintext. EncAccessToken/EncRefreshToken are excluded from
// JSON so API responses can never leak them.
type XeroConnection struct {
	OrganizationID  string    `json:"organization_id"`
	TenantID        string    `json:"tenant_id"`
	EncAccessToken  string    `json:"-"`
	EncRefreshToken string    `json:"-"`
	ExpiresAt       time.Time `json:"expires_at"`
	AutoPost        bool      `json:"auto_post"`
	Version         int64     `json:"version"` // optimistic concurrency token
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Validate checks the connection's shape.
func (c XeroConnection) Validate() error {
	if c.OrganizationID == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	return nil
}

// XeroAccountMapping maps a payroll component key to a Xero chart-of-
// accounts code. The well-known component key is "payroll.net_pay"; any
// other key is passed through untouched for future components.
type XeroAccountMapping struct {
	OrganizationID string `json:"organization_id"`
	ComponentKey   string `json:"component_key"`
	AccountCode    string `json:"account_code"`
}

// Validate checks the mapping's shape.
func (m XeroAccountMapping) Validate() error {
	if m.OrganizationID == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if m.ComponentKey == "" {
		return &ValidationError{Field: "component_key", Message: "required"}
	}
	if m.AccountCode == "" {
		return &ValidationError{Field: "account_code", Message: "required"}
	}
	return nil
}

// XeroContactMapping maps an employee to a Xero contact ID.
type XeroContactMapping struct {
	OrganizationID string `json:"organization_id"`
	EmployeeID     string `json:"employee_id"`
	ContactID      string `json:"contact_id"`
}

// Validate checks the mapping's shape.
func (m XeroContactMapping) Validate() error {
	if m.OrganizationID == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if m.EmployeeID == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	if m.ContactID == "" {
		return &ValidationError{Field: "contact_id", Message: "required"}
	}
	return nil
}

// XeroBillPostingStatus is the lifecycle of one bill posting attempt.
type XeroBillPostingStatus string

const (
	XeroBillPostingPending XeroBillPostingStatus = "pending"
	XeroBillPostingPosted  XeroBillPostingStatus = "posted"
	XeroBillPostingFailed  XeroBillPostingStatus = "failed"
)

// XeroBillPosting is one row of the posting ledger: one employee's bill for
// one payroll run. The (run_id, employee_id) pair makes posting idempotent
// — a run's bills post at most once per employee no matter how often the
// auto-post hook fires.
type XeroBillPosting struct {
	ID                string                `json:"id"`
	OrganizationID    string                `json:"organization_id"`
	RunID             string                `json:"run_id"`
	EmployeeID        string                `json:"employee_id"`
	Status            XeroBillPostingStatus `json:"status"`
	XeroInvoiceID     string                `json:"xero_invoice_id,omitempty"`
	XeroInvoiceNumber string                `json:"xero_invoice_number,omitempty"`
	Error             string                `json:"error,omitempty"`
	IdempotencyKey    string                `json:"idempotency_key,omitempty"`
	Version           int64                 `json:"version"` // optimistic concurrency token
	CreatedAt         time.Time             `json:"created_at"`
}

// Validate checks the posting's shape.
func (p XeroBillPosting) Validate() error {
	if p.OrganizationID == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if p.RunID == "" {
		return &ValidationError{Field: "run_id", Message: "required"}
	}
	switch p.Status {
	case XeroBillPostingPending, XeroBillPostingPosted, XeroBillPostingFailed:
	default:
		return &ValidationError{Field: "status", Message: "must be pending, posted, or failed"}
	}
	return nil
}

// XeroStore is the component 6 (Xero) storage contract: OAuth connections,
// account/contact mappings, and the bill-posting ledger.
//
// SaveXeroConnection is an upsert: a zero Version inserts, a nonzero
// Version updates iff it matches the stored version (ErrVersionConflict on
// mismatch). SaveXeroMappings replaces the org's full mapping sets
// atomically. RecordBillPosting inserts one ledger row; the caller keeps
// (run_id, employee_id) unique by checking ListBillPostings first.
//
// Implementations must be safe for concurrent use and must deep-copy values
// across the boundary.
type XeroStore interface {
	// GetXeroConnection returns the org's Xero connection or ErrNotFound.
	GetXeroConnection(ctx context.Context, orgID string) (XeroConnection, error)
	// SaveXeroConnection upserts the connection (version-checked update).
	SaveXeroConnection(ctx context.Context, c XeroConnection) (XeroConnection, error)
	// DeleteXeroConnection removes the connection and its mappings.
	// Idempotent: unknown orgs are a no-op.
	DeleteXeroConnection(ctx context.Context, orgID string) error
	// SaveXeroMappings atomically replaces the org's account and contact
	// mappings (empty slices clear the respective set).
	SaveXeroMappings(ctx context.Context, orgID string, accounts []XeroAccountMapping, contacts []XeroContactMapping) error
	// GetXeroMappings returns the org's mappings, each ordered by key.
	GetXeroMappings(ctx context.Context, orgID string) (accounts []XeroAccountMapping, contacts []XeroContactMapping, err error)
	// RecordBillPosting appends one ledger row. Version starts at 1.
	RecordBillPosting(ctx context.Context, p XeroBillPosting) (XeroBillPosting, error)
	// ListBillPostings returns the run's ledger rows ordered by
	// (created_at, id).
	ListBillPostings(ctx context.Context, orgID, runID string) ([]XeroBillPosting, error)
}
