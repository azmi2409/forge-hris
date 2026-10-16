package store

import (
	"context"
	"time"
)

// SCIMLink maps an IdP's SCIM externalId to one employee inside a tenant.
//
// The mapping is a separate record — not a column on employees — so SCIM
// concerns never leak into the core identity schema, and so it survives the
// soft-deactivate lifecycle: employees are never hard-deleted, so an IdP can
// re-correlate a re-provisioned user after DELETE (active=false) without
// losing the link.
type SCIMLink struct {
	OrganizationID string    `json:"organization_id"`
	ExternalID     string    `json:"external_id"`
	EmployeeID     string    `json:"employee_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// Validate checks the link's shape (the store assigns CreatedAt).
func (l SCIMLink) Validate() error {
	if l.OrganizationID == "" {
		return &ValidationError{Field: "organization_id", Message: "required"}
	}
	if l.ExternalID == "" {
		return &ValidationError{Field: "external_id", Message: "required"}
	}
	if l.EmployeeID == "" {
		return &ValidationError{Field: "employee_id", Message: "required"}
	}
	return nil
}

// SCIMLinkStore extends Store with the SCIM externalId mapping. It is a
// separate interface because the component-1 Store contract is stable (other
// workers build on it unchanged); *MemoryStore and *PostgresStore both
// implement it. Handlers type-assert the server store to this interface.
type SCIMLinkStore interface {
	Store

	// CreateSCIMLink records the mapping. The employee must exist.
	// Returns ErrAlreadyExists when the externalId is already linked in the
	// organization or the employee already carries a link; ErrNotFound for
	// an unknown organization or employee.
	CreateSCIMLink(ctx context.Context, l SCIMLink) (SCIMLink, error)
	// GetSCIMLinkByExternalID resolves an IdP identifier to its link, or
	// ErrNotFound when the organization has no such link.
	GetSCIMLinkByExternalID(ctx context.Context, orgID, externalID string) (SCIMLink, error)
	// GetSCIMLinkByEmployee returns the employee's link, or ErrNotFound when
	// the employee carries none.
	GetSCIMLinkByEmployee(ctx context.Context, orgID, employeeID string) (SCIMLink, error)
	// DeleteSCIMLinkByEmployee removes the employee's link. Idempotent:
	// deleting a link that does not exist is not an error.
	DeleteSCIMLinkByEmployee(ctx context.Context, orgID, employeeID string) error
}
