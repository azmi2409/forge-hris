package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func scanXeroConnection(r rowScanner) (XeroConnection, error) {
	var c XeroConnection
	var expiresAt *time.Time // nullable: unknown expiry reads as the zero time
	err := r.Scan(
		&c.OrganizationID, &c.TenantID, &c.EncAccessToken, &c.EncRefreshToken,
		&expiresAt, &c.AutoPost, &c.Version, &c.CreatedAt, &c.UpdatedAt,
	)
	if err == nil && expiresAt != nil {
		c.ExpiresAt = *expiresAt
	}
	return c, err
}

const xeroConnectionCols = `organization_id::text, tenant_id, enc_access_token, enc_refresh_token,
	expires_at, auto_post, version, created_at, updated_at`

func scanXeroBillPosting(r rowScanner) (XeroBillPosting, error) {
	var p XeroBillPosting
	err := r.Scan(
		&p.ID, &p.OrganizationID, &p.RunID, &p.EmployeeID, &p.Status,
		&p.XeroInvoiceID, &p.XeroInvoiceNumber, &p.Error, &p.IdempotencyKey,
		&p.Version, &p.CreatedAt,
	)
	return p, err
}

const xeroBillPostingCols = `id::text, organization_id::text, run_id::text, employee_id::text, status,
	xero_invoice_id, xero_invoice_number, error, idempotency_key, version, created_at`

// ---------------------------------------------------------------------------
// Xero connections
// ---------------------------------------------------------------------------

func (s *PostgresStore) GetXeroConnection(ctx context.Context, orgID string) (XeroConnection, error) {
	if err := ctx.Err(); err != nil {
		return XeroConnection{}, err
	}
	var out XeroConnection
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanXeroConnection(tx.QueryRow(ctx,
			`SELECT `+xeroConnectionCols+` FROM xero_connections WHERE organization_id = $1::uuid`, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: xero connection", ErrNotFound)
		}
		return err
	})
	if err != nil {
		return XeroConnection{}, err
	}
	return out, nil
}

func (s *PostgresStore) SaveXeroConnection(ctx context.Context, c XeroConnection) (XeroConnection, error) {
	if err := ctx.Err(); err != nil {
		return XeroConnection{}, err
	}
	if err := c.Validate(); err != nil {
		return XeroConnection{}, err
	}
	var out XeroConnection
	var expiresAt *time.Time
	if !c.ExpiresAt.IsZero() {
		expiresAt = &c.ExpiresAt
	}
	err := s.inTenant(ctx, c.OrganizationID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`,
			c.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: organization %s", ErrNotFound, c.OrganizationID)
		}
		if c.Version == 0 {
			var err error
			out, err = scanXeroConnection(tx.QueryRow(ctx, `
				INSERT INTO xero_connections (organization_id, tenant_id, enc_access_token, enc_refresh_token, expires_at, auto_post)
				VALUES ($1::uuid, $2, $3, $4, $5, $6)
				RETURNING `+xeroConnectionCols,
				c.OrganizationID, c.TenantID, c.EncAccessToken, c.EncRefreshToken, expiresAt, c.AutoPost))
			return mapPgError(err, "xero connection")
		}
		var err error
		out, err = scanXeroConnection(tx.QueryRow(ctx, `
			UPDATE xero_connections
			SET tenant_id = $3, enc_access_token = $4, enc_refresh_token = $5,
			    expires_at = $6, auto_post = $7, version = version + 1
			WHERE organization_id = $1::uuid AND version = $2
			RETURNING `+xeroConnectionCols,
			c.OrganizationID, c.Version, c.TenantID, c.EncAccessToken, c.EncRefreshToken, expiresAt, c.AutoPost))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: xero connection", ErrVersionConflict)
		}
		return err
	})
	if err != nil {
		return XeroConnection{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteXeroConnection(ctx context.Context, orgID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		// Mappings die with the connection; the posting ledger is kept for
		// audit (same semantics as the memory adapter).
		if _, err := tx.Exec(ctx,
			`DELETE FROM xero_account_mappings WHERE organization_id = $1::uuid`, orgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM xero_contact_mappings WHERE organization_id = $1::uuid`, orgID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM xero_connections WHERE organization_id = $1::uuid`, orgID)
		return err
	})
}

// ---------------------------------------------------------------------------
// Xero mappings
// ---------------------------------------------------------------------------

func (s *PostgresStore) SaveXeroMappings(ctx context.Context, orgID string, accounts []XeroAccountMapping, contacts []XeroContactMapping) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for i := range accounts {
		accounts[i].OrganizationID = orgID
		if err := accounts[i].Validate(); err != nil {
			return err
		}
	}
	for i := range contacts {
		contacts[i].OrganizationID = orgID
		if err := contacts[i].Validate(); err != nil {
			return err
		}
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`,
			orgID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: organization %s", ErrNotFound, orgID)
		}
		// Replace both sets atomically in one transaction.
		if _, err := tx.Exec(ctx,
			`DELETE FROM xero_account_mappings WHERE organization_id = $1::uuid`, orgID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM xero_contact_mappings WHERE organization_id = $1::uuid`, orgID); err != nil {
			return err
		}
		for _, a := range accounts {
			if _, err := tx.Exec(ctx,
				`INSERT INTO xero_account_mappings (organization_id, component_key, account_code)
				 VALUES ($1::uuid, $2, $3)`,
				orgID, a.ComponentKey, a.AccountCode); err != nil {
				return mapPgError(err, "xero account mapping")
			}
		}
		for _, c := range contacts {
			if _, err := tx.Exec(ctx,
				`INSERT INTO xero_contact_mappings (organization_id, employee_id, contact_id)
				 VALUES ($1::uuid, $2::uuid, $3)`,
				orgID, c.EmployeeID, c.ContactID); err != nil {
				return mapPgError(err, "xero contact mapping")
			}
		}
		return nil
	})
}

func (s *PostgresStore) GetXeroMappings(ctx context.Context, orgID string) ([]XeroAccountMapping, []XeroContactMapping, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var accounts []XeroAccountMapping
	var contacts []XeroContactMapping
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT organization_id::text, component_key, account_code
			 FROM xero_account_mappings WHERE organization_id = $1::uuid
			 ORDER BY component_key`, orgID)
		if err != nil {
			return err
		}
		accounts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (XeroAccountMapping, error) {
			var m XeroAccountMapping
			err := r.Scan(&m.OrganizationID, &m.ComponentKey, &m.AccountCode)
			return m, err
		})
		if err != nil {
			return err
		}
		rows, err = tx.Query(ctx,
			`SELECT organization_id::text, employee_id::text, contact_id
			 FROM xero_contact_mappings WHERE organization_id = $1::uuid
			 ORDER BY employee_id`, orgID)
		if err != nil {
			return err
		}
		contacts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (XeroContactMapping, error) {
			var m XeroContactMapping
			err := r.Scan(&m.OrganizationID, &m.EmployeeID, &m.ContactID)
			return m, err
		})
		return err
	})
	if err != nil {
		return nil, nil, fmt.Errorf("store: get xero mappings: %w", err)
	}
	return accounts, contacts, nil
}

// ---------------------------------------------------------------------------
// Xero bill postings
// ---------------------------------------------------------------------------

func (s *PostgresStore) RecordBillPosting(ctx context.Context, p XeroBillPosting) (XeroBillPosting, error) {
	if err := ctx.Err(); err != nil {
		return XeroBillPosting{}, err
	}
	if err := p.Validate(); err != nil {
		return XeroBillPosting{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	var out XeroBillPosting
	err := s.inTenant(ctx, p.OrganizationID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`,
			p.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: organization %s", ErrNotFound, p.OrganizationID)
		}
		var err error
		out, err = scanXeroBillPosting(tx.QueryRow(ctx, `
			INSERT INTO xero_bill_postings (id, organization_id, run_id, employee_id, status,
				xero_invoice_id, xero_invoice_number, error, idempotency_key)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9)
			RETURNING `+xeroBillPostingCols,
			p.ID, p.OrganizationID, p.RunID, p.EmployeeID, p.Status,
			p.XeroInvoiceID, p.XeroInvoiceNumber, p.Error, p.IdempotencyKey))
		return mapPgError(err, "xero bill posting")
	})
	if err != nil {
		return XeroBillPosting{}, err
	}
	// The unique (organization_id, run_id, employee_id) index makes a
	// duplicate posting a conflict, matching the memory adapter's
	// ErrAlreadyExists.
	return out, nil
}

func (s *PostgresStore) ListBillPostings(ctx context.Context, orgID, runID string) ([]XeroBillPosting, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []XeroBillPosting
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+xeroBillPostingCols+` FROM xero_bill_postings
			WHERE organization_id = $1::uuid AND run_id = $2::uuid
			ORDER BY created_at, id`, orgID, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (XeroBillPosting, error) {
			return scanXeroBillPosting(r)
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: list bill postings: %w", err)
	}
	return out, nil
}
