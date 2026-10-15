package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the production Store backed by Postgres + pgxpool.
//
// It implements the exact same contract as MemoryStore: UUID identities,
// optimistic concurrency via the version column (UPDATE ... WHERE id AND
// version, 0 rows -> ErrVersionConflict/ErrNotFound), soft employee deletes,
// and a hash-chained audit log whose appends serialize per org on a
// transaction-scoped advisory lock (pg_advisory_xact_lock). NOTE: a plain
// SELECT ... FOR UPDATE on the tail row is NOT sufficient here — under READ
// COMMITTED a waiter re-checks the row it blocked on instead of re-finding
// the newest tail, so concurrent appends could fork the chain.
//
// Tenancy: every org-scoped operation runs inside a transaction that first
// executes SELECT set_config('app.tenant_id', $1, true) (SET LOCAL
// semantics), activating the tenant_isolation RLS policies, and every query
// additionally carries an explicit organization_id = $1 predicate.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// Compile-time contract check.
var _ Store = (*PostgresStore)(nil)

// NewPostgresStore connects to Postgres and verifies the schema is present.
// Run ApplyMigration first on a fresh database.
func NewPostgresStore(ctx context.Context, databaseURL string) (*PostgresStore, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: bad DATABASE_URL: %w", err)
	}
	// Pool sizing for agent fleets: many short-lived queries, so keep enough
	// connections without overwhelming Postgres.
	if cfg.MaxConns == 0 || cfg.MaxConns == 4 {
		cfg.MaxConns = 32
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	var ok bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'organizations')`,
	).Scan(&ok)
	if err != nil || !ok {
		pool.Close()
		return nil, fmt.Errorf("store: organizations table missing (run ApplyMigration)")
	}
	return &PostgresStore{pool: pool}, nil
}

// Close releases the pool.
func (s *PostgresStore) Close() { s.pool.Close() }

// inTenant runs fn inside a transaction with app.tenant_id set to orgID.
// RLS policies activate on this setting; queries still carry explicit
// organization_id predicates (belt and suspenders).
func (s *PostgresStore) inTenant(ctx context.Context, orgID string, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, orgID); err != nil {
		return fmt.Errorf("store: set tenant: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// mapPgError translates Postgres constraint violations to sentinel errors.
func mapPgError(err error, what string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%w: %s", ErrAlreadyExists, what)
		case "23503": // foreign_key_violation
			return fmt.Errorf("%w: %s", ErrNotFound, what)
		}
	}
	return err
}

// versionOutcome maps a 0-row version-checked write to ErrNotFound (the row
// is gone) or ErrVersionConflict (the row exists but the version is stale).
// table must be one of the versioned tables; orgID scopes tenant tables,
// empty orgID means the global organizations table.
func versionOutcome(ctx context.Context, tx pgx.Tx, table, orgID, id string) error {
	var exists bool
	var err error
	if orgID == "" {
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`, id).Scan(&exists)
	} else {
		q := fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = $1::uuid AND organization_id = $2::uuid)`, table)
		err = tx.QueryRow(ctx, q, id, orgID).Scan(&exists)
	}
	if err != nil {
		return fmt.Errorf("store: existence check: %w", err)
	}
	if !exists {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return fmt.Errorf("%w: %s", ErrVersionConflict, id)
}

// ---------------------------------------------------------------------------
// Row scanners. UUID columns are selected as ::text so they scan straight
// into Go strings.
// ---------------------------------------------------------------------------

// rowScanner is the Scan half shared by pgx.Row and pgx.CollectableRow.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanOrganization(r rowScanner) (Organization, error) {
	var o Organization
	err := r.Scan(&o.ID, &o.Name, &o.Slug, &o.CountryCode, &o.Version, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}

const organizationCols = `id::text, name, slug, country_code, version, created_at, updated_at`

func scanGroup(r rowScanner) (Group, error) {
	var g Group
	err := r.Scan(&g.ID, &g.OrganizationID, &g.Name, &g.ParentID, &g.Version, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

const groupCols = `id::text, organization_id::text, name, parent_id::text, version, created_at, updated_at`

func scanEmployee(r rowScanner) (Employee, error) {
	var e Employee
	err := r.Scan(
		&e.ID, &e.OrganizationID, &e.EmployeeNumber, &e.FirstName, &e.LastName,
		&e.Email, &e.Position, &e.EmploymentType, &e.GroupID, &e.ManagerID,
		&e.TimeZone, &e.WorkStartDate, &e.Status, &e.Version, &e.CreatedAt, &e.UpdatedAt,
	)
	return e, err
}

const employeeCols = `id::text, organization_id::text, employee_number, first_name, last_name,
	email, position, employment_type, group_id::text, manager_id::text,
	time_zone, work_start_date, status, version, created_at, updated_at`

func scanCredential(r rowScanner, withHash bool) (Credential, error) {
	var c Credential
	if withHash {
		err := r.Scan(
			&c.ID, &c.OrganizationID, &c.Kind, &c.Name, &c.TokenHash, &c.EmployeeID,
			&c.Scopes, &c.ExpiresAt, &c.RevokedAt, &c.LastUsedAt, &c.CreatedAt, &c.Version,
		)
		return c, err
	}
	err := r.Scan(
		&c.ID, &c.OrganizationID, &c.Kind, &c.Name, &c.EmployeeID,
		&c.Scopes, &c.ExpiresAt, &c.RevokedAt, &c.LastUsedAt, &c.CreatedAt, &c.Version,
	)
	return c, err
}

const credentialCols = `id::text, organization_id::text, kind, name, employee_id::text,
	scopes, expires_at, revoked_at, last_used_at, created_at, version`

// credentialColsWithHash matches scanCredential's withHash=true column order
// (token_hash right after name).
const credentialColsWithHash = `id::text, organization_id::text, kind, name, token_hash,
	employee_id::text, scopes, expires_at, revoked_at, last_used_at, created_at, version`

func scanAuditEvent(r rowScanner) (AuditEvent, error) {
	var e AuditEvent
	var before, after []byte
	err := r.Scan(
		&e.ID, &e.OrganizationID, &e.ActorType, &e.ActorID, &e.OnBehalfOf,
		&e.Action, &e.ResourceType, &e.ResourceID, &before, &after,
		&e.IdempotencyKey, &e.PrevHash, &e.Hash, &e.CreatedAt,
	)
	if err != nil {
		return AuditEvent{}, err
	}
	e.Before = json.RawMessage(before)
	e.After = json.RawMessage(after)
	return e, nil
}

const auditCols = `id, organization_id::text, actor_type, actor_id, on_behalf_of,
	action, resource_type, resource_id, before, after,
	idempotency_key, prev_hash, hash, created_at`

// ---------------------------------------------------------------------------
// Organizations
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateOrganization(ctx context.Context, o Organization) (Organization, error) {
	if err := ctx.Err(); err != nil {
		return Organization{}, err
	}
	if err := o.Validate(); err != nil {
		return Organization{}, err
	}
	if o.ID == "" {
		o.ID = newID()
	}
	out, err := scanOrganization(s.pool.QueryRow(ctx, `
		INSERT INTO organizations (id, name, slug, country_code)
		VALUES ($1::uuid, $2, $3, $4)
		RETURNING `+organizationCols, o.ID, o.Name, o.Slug, o.CountryCode))
	if err != nil {
		return Organization{}, mapPgError(err, "organization")
	}
	return out, nil
}

func (s *PostgresStore) GetOrganization(ctx context.Context, id string) (Organization, error) {
	if err := ctx.Err(); err != nil {
		return Organization{}, err
	}
	out, err := scanOrganization(s.pool.QueryRow(ctx,
		`SELECT `+organizationCols+` FROM organizations WHERE id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, fmt.Errorf("%w: organization %s", ErrNotFound, id)
	}
	return out, err
}

func (s *PostgresStore) GetOrganizationBySlug(ctx context.Context, slug string) (Organization, error) {
	if err := ctx.Err(); err != nil {
		return Organization{}, err
	}
	out, err := scanOrganization(s.pool.QueryRow(ctx,
		`SELECT `+organizationCols+` FROM organizations WHERE slug = $1`, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, fmt.Errorf("%w: organization slug %q", ErrNotFound, slug)
	}
	return out, err
}

func (s *PostgresStore) UpdateOrganization(ctx context.Context, o Organization) (Organization, error) {
	if err := ctx.Err(); err != nil {
		return Organization{}, err
	}
	if err := o.Validate(); err != nil {
		return Organization{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Organization{}, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	out, err := scanOrganization(tx.QueryRow(ctx, `
		UPDATE organizations
		SET name = $2, slug = $3, country_code = $4, version = version + 1
		WHERE id = $1::uuid AND version = $5
		RETURNING `+organizationCols, o.ID, o.Name, o.Slug, o.CountryCode, o.Version))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Organization{}, versionOutcome(ctx, tx, "", "", o.ID)
		}
		return Organization{}, mapPgError(err, "organization")
	}
	if err := tx.Commit(ctx); err != nil {
		return Organization{}, fmt.Errorf("store: commit: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) ListOrganizations(ctx context.Context, p Page) ([]Organization, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(p.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if p.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+organizationCols+` FROM organizations
		WHERE (NOT $1 OR (created_at, id) > ($2::timestamptz, NULLIF($3,'')::uuid))
		ORDER BY created_at, id
		LIMIT $4`, haveCursor, ct, cid, limit)
	if err != nil {
		return nil, "", fmt.Errorf("store: list organizations: %w", err)
	}
	defer rows.Close()
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Organization, error) { return scanOrganization(r) })
	if err != nil {
		return nil, "", fmt.Errorf("store: list organizations: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Groups
// ---------------------------------------------------------------------------

// groupCycleTx reports whether setting group's parent to parentID would
// create a cycle (true when parentID is the group itself or one of its
// descendants).
func groupCycleTx(ctx context.Context, tx pgx.Tx, orgID, groupID, parentID string) (bool, error) {
	var cycle bool
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE descendants(id) AS (
			SELECT id FROM groups WHERE id = $1::uuid AND organization_id = $2::uuid
			UNION
			SELECT g.id FROM groups g JOIN descendants d ON g.parent_id = d.id
		)
		SELECT EXISTS(SELECT 1 FROM descendants WHERE id = $3::uuid)`,
		groupID, orgID, parentID).Scan(&cycle)
	return cycle, err
}

func (s *PostgresStore) CreateGroup(ctx context.Context, g Group) (Group, error) {
	if err := ctx.Err(); err != nil {
		return Group{}, err
	}
	if err := g.Validate(); err != nil {
		return Group{}, err
	}
	if g.ID == "" {
		g.ID = newID()
	}
	var out Group
	err := s.inTenant(ctx, g.OrganizationID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`,
			g.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: organization %s", ErrNotFound, g.OrganizationID)
		}
		if g.ParentID != nil {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*g.ParentID, g.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: parent group %s", ErrNotFound, *g.ParentID)
			}
		}
		var err error
		out, err = scanGroup(tx.QueryRow(ctx, `
			INSERT INTO groups (id, organization_id, name, parent_id)
			VALUES ($1::uuid, $2::uuid, $3, $4::uuid)
			RETURNING `+groupCols, g.ID, g.OrganizationID, g.Name, g.ParentID))
		if err != nil {
			return mapPgError(err, "group")
		}
		return nil
	})
	if err != nil {
		return Group{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetGroup(ctx context.Context, orgID, id string) (Group, error) {
	if err := ctx.Err(); err != nil {
		return Group{}, err
	}
	var out Group
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanGroup(tx.QueryRow(ctx,
			`SELECT `+groupCols+` FROM groups WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: group %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return Group{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateGroup(ctx context.Context, g Group) (Group, error) {
	if err := ctx.Err(); err != nil {
		return Group{}, err
	}
	if err := g.Validate(); err != nil {
		return Group{}, err
	}
	var out Group
	err := s.inTenant(ctx, g.OrganizationID, func(tx pgx.Tx) error {
		if g.ParentID != nil {
			var ok bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*g.ParentID, g.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: parent group %s", ErrNotFound, *g.ParentID)
			}
			cycle, err := groupCycleTx(ctx, tx, g.OrganizationID, g.ID, *g.ParentID)
			if err != nil {
				return err
			}
			if cycle {
				return &ValidationError{Field: "parent_id", Message: "would create a cycle"}
			}
		}
		var err error
		out, err = scanGroup(tx.QueryRow(ctx, `
			UPDATE groups
			SET name = $2, parent_id = $3::uuid, version = version + 1
			WHERE id = $1::uuid AND organization_id = $4::uuid AND version = $5
			RETURNING `+groupCols, g.ID, g.Name, g.ParentID, g.OrganizationID, g.Version))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "groups", g.OrganizationID, g.ID)
			}
			return mapPgError(err, "group")
		}
		return nil
	})
	if err != nil {
		return Group{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteGroup(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1::uuid AND organization_id = $2::uuid)`,
			id, orgID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: group %s", ErrNotFound, id)
		}
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM employees WHERE group_id = $1::uuid AND organization_id = $2::uuid)`,
			id, orgID).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%w: group %s still has members", ErrNotEmpty, id)
		}
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM groups WHERE parent_id = $1::uuid AND organization_id = $2::uuid)`,
			id, orgID).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%w: group %s still has child groups", ErrNotEmpty, id)
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM groups WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		return err
	})
}

func (s *PostgresStore) ListGroups(ctx context.Context, orgID string, p Page) ([]Group, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(p.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if p.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []Group
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+groupCols+` FROM groups
			WHERE organization_id = $1::uuid
			  AND (NOT $2 OR (created_at, id) > ($3::timestamptz, NULLIF($4,'')::uuid))
			ORDER BY created_at, id
			LIMIT $5`, orgID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Group, error) { return scanGroup(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list groups: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Employees
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateEmployee(ctx context.Context, e Employee) (Employee, error) {
	if err := ctx.Err(); err != nil {
		return Employee{}, err
	}
	if err := e.Validate(); err != nil {
		return Employee{}, err
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.Status == "" {
		e.Status = InvitationNotInvited
	}
	if e.EmploymentType == "" {
		e.EmploymentType = EmploymentFullTime
	}
	var out Employee
	err := s.inTenant(ctx, e.OrganizationID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`,
			e.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: organization %s", ErrNotFound, e.OrganizationID)
		}
		if e.GroupID != nil {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*e.GroupID, e.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: group %s", ErrNotFound, *e.GroupID)
			}
		}
		if e.ManagerID != nil {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*e.ManagerID, e.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: manager %s", ErrNotFound, *e.ManagerID)
			}
		}
		var err error
		out, err = scanEmployee(tx.QueryRow(ctx, `
			INSERT INTO employees
				(id, organization_id, employee_number, first_name, last_name, email,
				 position, employment_type, group_id, manager_id, time_zone,
				 work_start_date, status)
			VALUES
				($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::uuid, $10::uuid, $11, $12, $13)
			RETURNING `+employeeCols,
			e.ID, e.OrganizationID, e.EmployeeNumber, e.FirstName, e.LastName, e.Email,
			e.Position, string(e.EmploymentType), e.GroupID, e.ManagerID, e.TimeZone,
			e.WorkStartDate, string(e.Status)))
		if err != nil {
			return mapPgError(err, "employee")
		}
		return nil
	})
	if err != nil {
		return Employee{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetEmployee(ctx context.Context, orgID, id string) (Employee, error) {
	if err := ctx.Err(); err != nil {
		return Employee{}, err
	}
	var out Employee
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEmployee(tx.QueryRow(ctx,
			`SELECT `+employeeCols+` FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: employee %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return Employee{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateEmployee(ctx context.Context, e Employee) (Employee, error) {
	if err := ctx.Err(); err != nil {
		return Employee{}, err
	}
	if err := e.Validate(); err != nil {
		return Employee{}, err
	}
	var out Employee
	err := s.inTenant(ctx, e.OrganizationID, func(tx pgx.Tx) error {
		if e.GroupID != nil {
			var ok bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM groups WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*e.GroupID, e.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: group %s", ErrNotFound, *e.GroupID)
			}
		}
		if e.ManagerID != nil {
			if *e.ManagerID == e.ID {
				return &ValidationError{Field: "manager_id", Message: "cannot manage self"}
			}
			var ok bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*e.ManagerID, e.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: manager %s", ErrNotFound, *e.ManagerID)
			}
		}
		var err error
		out, err = scanEmployee(tx.QueryRow(ctx, `
			UPDATE employees
			SET employee_number = $3, first_name = $4, last_name = $5, email = $6,
			    position = $7, employment_type = $8, group_id = $9::uuid,
			    manager_id = $10::uuid, time_zone = $11, work_start_date = $12,
			    status = $13, version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $14
			RETURNING `+employeeCols,
			e.ID, e.OrganizationID, e.EmployeeNumber, e.FirstName, e.LastName, e.Email,
			e.Position, string(e.EmploymentType), e.GroupID, e.ManagerID, e.TimeZone,
			e.WorkStartDate, string(e.Status), e.Version))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "employees", e.OrganizationID, e.ID)
			}
			return mapPgError(err, "employee")
		}
		return nil
	})
	if err != nil {
		return Employee{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeactivateEmployee(ctx context.Context, orgID, id string, version int64) (Employee, error) {
	if err := ctx.Err(); err != nil {
		return Employee{}, err
	}
	var out Employee
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEmployee(tx.QueryRow(ctx, `
			UPDATE employees
			SET status = 'removed', version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $3
			RETURNING `+employeeCols, id, orgID, version))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "employees", orgID, id)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return Employee{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListEmployees(ctx context.Context, orgID string, f EmployeeFilter, p Page) ([]Employee, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(p.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if p.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []Employee
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+employeeCols+` FROM employees
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR group_id = NULLIF($2,'')::uuid)
			  AND ($3 = '' OR status = $3)
			  AND ($4 = '' OR first_name ILIKE '%'||$4||'%'
			               OR last_name ILIKE '%'||$4||'%'
			               OR email ILIKE '%'||$4||'%'
			               OR employee_number ILIKE '%'||$4||'%')
			  AND (NOT $5 OR (created_at, id) > ($6::timestamptz, NULLIF($7,'')::uuid))
			ORDER BY created_at, id
			LIMIT $8`,
			orgID, f.GroupID, string(f.Status), f.Search, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Employee, error) { return scanEmployee(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list employees: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Memberships
// ---------------------------------------------------------------------------

func (s *PostgresStore) SetRole(ctx context.Context, orgID, employeeID string, role Role, grantedBy string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !ValidRole(role) {
		return &ValidationError{Field: "role", Message: "unknown role"}
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx,
			`SELECT status FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid`,
			employeeID, orgID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
		}
		if err != nil {
			return err
		}
		if status != string(InvitationJoined) {
			return fmt.Errorf("store: employee %s has not joined (status %s)", employeeID, status)
		}
		var gb *string
		if grantedBy != "" {
			gb = &grantedBy
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO memberships (organization_id, employee_id, role, granted_by)
			VALUES ($1::uuid, $2::uuid, $3, $4)
			ON CONFLICT (organization_id, employee_id)
			DO UPDATE SET role = EXCLUDED.role, granted_by = EXCLUDED.granted_by`,
			orgID, employeeID, string(role), gb)
		return err
	})
}

func (s *PostgresStore) GetRole(ctx context.Context, orgID, employeeID string) (Role, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var role string
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT role FROM memberships WHERE organization_id = $1::uuid AND employee_id = $2::uuid`,
			orgID, employeeID).Scan(&role)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: membership for employee %s", ErrNotFound, employeeID)
	}
	if err != nil {
		return "", err
	}
	return Role(role), nil
}

func (s *PostgresStore) ListMemberships(ctx context.Context, orgID string, p Page) ([]Membership, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(p.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if p.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []Membership
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT organization_id::text, employee_id::text, role, granted_by, created_at
			FROM memberships
			WHERE organization_id = $1::uuid
			  AND (NOT $2 OR (created_at, employee_id) > ($3::timestamptz, NULLIF($4,'')::uuid))
			ORDER BY created_at, employee_id
			LIMIT $5`, orgID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Membership
			if err := rows.Scan(&m.OrganizationID, &m.EmployeeID, &m.Role, &m.GrantedBy, &m.CreatedAt); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list memberships: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.EmployeeID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func (s *PostgresStore) IssueCredential(ctx context.Context, c Credential, tokenPlaintext string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if err := c.Validate(); err != nil {
		return Credential{}, err
	}
	if tokenPlaintext == "" {
		return Credential{}, &ValidationError{Field: "token", Message: "plaintext token required"}
	}
	c.TokenHash = credentialHash(tokenPlaintext)
	if c.ID == "" {
		c.ID = newID()
	}
	scopes := c.Scopes
	if scopes == nil {
		scopes = []string{} // explicit NULL would violate the NOT NULL default
	}
	var out Credential
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
		if c.EmployeeID != nil {
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*c.EmployeeID, c.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: employee %s", ErrNotFound, *c.EmployeeID)
			}
		}
		var err error
		out, err = scanCredential(tx.QueryRow(ctx, `
			INSERT INTO credentials
				(id, organization_id, kind, name, token_hash, employee_id, scopes, expires_at)
			VALUES
				($1::uuid, $2::uuid, $3, $4, $5, $6::uuid, $7, $8)
			RETURNING `+credentialColsWithHash,
			c.ID, c.OrganizationID, string(c.Kind), c.Name, c.TokenHash,
			c.EmployeeID, scopes, c.ExpiresAt), true)
		if err != nil {
			return mapPgError(err, "credential")
		}
		return nil
	})
	if err != nil {
		return Credential{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetCredentialByTokenHash(ctx context.Context, hash string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	out, err := scanCredential(s.pool.QueryRow(ctx,
		`SELECT `+credentialColsWithHash+` FROM credentials WHERE token_hash = $1`, hash), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, fmt.Errorf("%w: credential", ErrNotFound)
	}
	if err != nil {
		return Credential{}, err
	}
	if out.RevokedAt != nil {
		return Credential{}, fmt.Errorf("%w: credential %s revoked", ErrCredentialInvalid, out.ID)
	}
	if out.ExpiresAt != nil && !out.ExpiresAt.After(time.Now()) {
		return Credential{}, fmt.Errorf("%w: credential %s expired", ErrCredentialInvalid, out.ID)
	}
	return out, nil
}

func (s *PostgresStore) GetCredential(ctx context.Context, orgID, id string) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	var out Credential
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		// The token hash is internal-only; the admin read path doesn't need it.
		out, err = scanCredential(tx.QueryRow(ctx,
			`SELECT `+credentialCols+` FROM credentials WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID), false)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: credential %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return Credential{}, err
	}
	return out, nil
}

func (s *PostgresStore) RevokeCredential(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE credentials
			SET revoked_at = COALESCE(revoked_at, now())
			WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: credential %s", ErrNotFound, id)
		}
		return nil
	})
}

func (s *PostgresStore) TouchCredential(ctx context.Context, orgID, id string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE credentials SET last_used_at = $3 WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID, at)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: credential %s", ErrNotFound, id)
		}
		return nil
	})
}

func (s *PostgresStore) ListCredentials(ctx context.Context, orgID string, p Page) ([]Credential, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(p.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if p.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []Credential
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+credentialCols+` FROM credentials
			WHERE organization_id = $1::uuid
			  AND (NOT $2 OR (created_at, id) > ($3::timestamptz, NULLIF($4,'')::uuid))
			ORDER BY created_at, id
			LIMIT $5`, orgID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Credential, error) { return scanCredential(r, false) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list credentials: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

func (s *PostgresStore) AppendAuditEvent(ctx context.Context, e AuditEvent) (AuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return AuditEvent{}, err
	}
	if err := e.Validate(); err != nil {
		return AuditEvent{}, err
	}
	err := s.inTenant(ctx, e.OrganizationID, func(tx pgx.Tx) error {
		// Serialize appends per org on an advisory lock. A SELECT ... FOR UPDATE
		// on the tail row is not enough: under READ COMMITTED a waiter
		// re-checks the row it blocked on rather than re-finding the newest
		// tail, so two concurrent appends could both link to the same parent
		// and fork the chain. The advisory lock makes tail-read + insert
		// atomic per org.
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext('forge-hris:audit:' || $1::text))`,
			e.OrganizationID); err != nil {
			return fmt.Errorf("store: audit append lock: %w", err)
		}
		// Read the org's tail; the advisory lock above guarantees no
		// concurrent append can slip a new tail in underneath us.
		var prevHash string
		err := tx.QueryRow(ctx,
			`SELECT hash FROM audit_events WHERE organization_id = $1::uuid ORDER BY id DESC LIMIT 1 FOR UPDATE`,
			e.OrganizationID).Scan(&prevHash)
		if errors.Is(err, pgx.ErrNoRows) {
			prevHash = ""
		} else if err != nil {
			return err
		}
		e.CreatedAt = time.Now().UTC()
		e.PrevHash = prevHash
		hash, err := computeAuditHash(prevHash, e)
		if err != nil {
			return err
		}
		e.Hash = hash
		return tx.QueryRow(ctx, `
			INSERT INTO audit_events
				(organization_id, actor_type, actor_id, on_behalf_of, action,
				 resource_type, resource_id, before, after, idempotency_key,
				 prev_hash, hash, created_at)
			VALUES
				($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			RETURNING id`,
			e.OrganizationID, string(e.ActorType), e.ActorID, e.OnBehalfOf, e.Action,
			e.ResourceType, e.ResourceID, e.Before, e.After, e.IdempotencyKey,
			e.PrevHash, e.Hash, e.CreatedAt).Scan(&e.ID)
	})
	if err != nil {
		return AuditEvent{}, err
	}
	return e, nil
}

func (s *PostgresStore) ListAuditEvents(ctx context.Context, orgID string, f AuditFilter, p Page) ([]AuditEvent, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(p.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if p.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(p.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var since *time.Time
	if f.Since != nil {
		since = f.Since
	}
	var out []AuditEvent
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+auditCols+` FROM audit_events
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR actor_type = $2)
			  AND ($3 = '' OR action = $3)
			  AND ($4 = '' OR resource_type = $4)
			  AND ($5::timestamptz IS NULL OR created_at >= $5::timestamptz)
			  AND (NOT $6 OR (created_at, id) > ($7::timestamptz, NULLIF($8,'')::bigint))
			ORDER BY created_at, id
			LIMIT $9`,
			orgID, string(f.ActorType), f.Action, f.ResourceType, since,
			haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (AuditEvent, error) { return scanAuditEvent(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list audit events: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, strconv.FormatInt(last.ID, 10))
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Idempotency
// ---------------------------------------------------------------------------

func (s *PostgresStore) IdempotencyGet(ctx context.Context, orgID, key string) (IdempotencyRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return IdempotencyRecord{}, false, err
	}
	var out IdempotencyRecord
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT organization_id::text, key, fingerprint, status_code, response_body, created_at
			FROM idempotency_records
			WHERE organization_id = $1::uuid AND key = $2
			  AND created_at > now() - interval '24 hours'`,
			orgID, key).Scan(
			&out.OrganizationID, &out.Key, &out.Fingerprint,
			&out.StatusCode, &out.ResponseBody, &out.CreatedAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return IdempotencyRecord{}, false, err
	}
	return out, true, nil
}

func (s *PostgresStore) IdempotencyPut(ctx context.Context, r IdempotencyRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	return s.inTenant(ctx, r.OrganizationID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO idempotency_records
				(organization_id, key, fingerprint, status_code, response_body)
			VALUES ($1::uuid, $2, $3, $4, $5)
			ON CONFLICT (organization_id, key)
			DO UPDATE SET fingerprint = EXCLUDED.fingerprint,
			              status_code = EXCLUDED.status_code,
			              response_body = EXCLUDED.response_body,
			              created_at = now()`,
			r.OrganizationID, r.Key, r.Fingerprint, r.StatusCode, r.ResponseBody)
		return err
	})
}
