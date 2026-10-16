package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PostgresStore SCIM link implementation (component 4, Worker C), backed by
// the scim_links table (see migrations/005_scim.sql). Every operation runs
// inside inTenant so the tenant_isolation RLS policy is active, and every
// query carries an explicit organization_id predicate (belt and suspenders).
//
// NOTE: not covered by automated tests — this environment has no Postgres
// (see postgres_test.go skip convention). Compile-checked only; verify
// against a live database before trusting it in production.

// Compile-time contract check.
var _ SCIMLinkStore = (*PostgresStore)(nil)

func scanSCIMLink(row pgx.Row) (SCIMLink, error) {
	var l SCIMLink
	if err := row.Scan(&l.OrganizationID, &l.ExternalID, &l.EmployeeID, &l.CreatedAt); err != nil {
		return SCIMLink{}, err
	}
	return l, nil
}

func (s *PostgresStore) CreateSCIMLink(ctx context.Context, l SCIMLink) (SCIMLink, error) {
	if err := ctx.Err(); err != nil {
		return SCIMLink{}, err
	}
	if err := l.Validate(); err != nil {
		return SCIMLink{}, err
	}
	var created SCIMLink
	err := s.inTenant(ctx, l.OrganizationID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO scim_links (organization_id, external_id, employee_id)
			VALUES ($1, $2, $3)
			RETURNING organization_id, external_id, employee_id, created_at`,
			l.OrganizationID, l.ExternalID, l.EmployeeID)
		var err error
		created, err = scanSCIMLink(row)
		if err != nil {
			// 23505 unique_violation -> ErrAlreadyExists (duplicate
			// (org, external_id) or a second link for the employee);
			// 23503 foreign_key_violation -> ErrNotFound (unknown org or
			// employee).
			return mapPgError(err, fmt.Sprintf("scim link %q", l.ExternalID))
		}
		return nil
	})
	if err != nil {
		return SCIMLink{}, err
	}
	return created, nil
}

func (s *PostgresStore) GetSCIMLinkByExternalID(ctx context.Context, orgID, externalID string) (SCIMLink, error) {
	if err := ctx.Err(); err != nil {
		return SCIMLink{}, err
	}
	var l SCIMLink
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT organization_id, external_id, employee_id, created_at
			FROM scim_links
			WHERE organization_id = $1 AND external_id = $2`,
			orgID, externalID)
		var err error
		l, err = scanSCIMLink(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: scim externalId %q", ErrNotFound, externalID)
		}
		return err
	})
	if err != nil {
		return SCIMLink{}, err
	}
	return l, nil
}

func (s *PostgresStore) GetSCIMLinkByEmployee(ctx context.Context, orgID, employeeID string) (SCIMLink, error) {
	if err := ctx.Err(); err != nil {
		return SCIMLink{}, err
	}
	var l SCIMLink
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT organization_id, external_id, employee_id, created_at
			FROM scim_links
			WHERE organization_id = $1 AND employee_id = $2`,
			orgID, employeeID)
		var err error
		l, err = scanSCIMLink(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: scim link for employee %s", ErrNotFound, employeeID)
		}
		return err
	})
	if err != nil {
		return SCIMLink{}, err
	}
	return l, nil
}

func (s *PostgresStore) DeleteSCIMLinkByEmployee(ctx context.Context, orgID, employeeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		// Idempotent: zero rows deleted is not an error.
		_, err := tx.Exec(ctx, `
			DELETE FROM scim_links
			WHERE organization_id = $1 AND employee_id = $2`,
			orgID, employeeID)
		return err
	})
}
