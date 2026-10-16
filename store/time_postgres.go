package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------------------
// Row scanners for component 2 tables. UUID columns are selected as ::text
// so they scan straight into Go strings; nullable UUIDs scan into *string
// (NULL -> nil).
// ---------------------------------------------------------------------------

func scanProject(r rowScanner) (Project, error) {
	var p Project
	err := r.Scan(&p.ID, &p.OrganizationID, &p.Name, &p.ClientName, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

const projectCols = `id::text, organization_id::text, name, client_name, version, created_at, updated_at`

func scanTimeEntry(r rowScanner) (TimeEntry, error) {
	var e TimeEntry
	err := r.Scan(
		&e.ID, &e.OrganizationID, &e.EmployeeID, &e.Kind, &e.OccurredAt,
		&e.ProjectID, &e.ActivityID, &e.LocationID, &e.Latitude, &e.Longitude, &e.Address,
		&e.ClientType, &e.PrevID, &e.NextID, &e.IsLocked, &e.Version,
		&e.CreatedAt, &e.UpdatedAt, &e.DeletedAt,
	)
	return e, err
}

const timeEntryCols = `id::text, organization_id::text, employee_id::text, kind, occurred_at,
	project_id::text, activity_id::text, location_id::text, latitude, longitude, address,
	client_type, prev_id::text, next_id::text, is_locked, version,
	created_at, updated_at, deleted_at`

func scanHourEntry(r rowScanner) (HourEntry, error) {
	var e HourEntry
	err := r.Scan(
		&e.ID, &e.OrganizationID, &e.EmployeeID, &e.WorkDate, &e.Hours,
		&e.ProjectID, &e.Note, &e.Version, &e.CreatedAt, &e.UpdatedAt, &e.DeletedAt,
	)
	return e, err
}

const hourEntryCols = `id::text, organization_id::text, employee_id::text, work_date, hours,
	project_id::text, note, version, created_at, updated_at, deleted_at`

func scanPayPeriodDefinition(r rowScanner) (PayPeriodDefinition, error) {
	var d PayPeriodDefinition
	err := r.Scan(&d.ID, &d.OrganizationID, &d.Name, &d.Cadence, &d.AnchorDay, &d.Version, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

const payPeriodDefinitionCols = `id::text, organization_id::text, name, cadence, anchor_day, version, created_at, updated_at`

func scanPayPeriod(r rowScanner) (PayPeriod, error) {
	var p PayPeriod
	err := r.Scan(&p.ID, &p.OrganizationID, &p.DefinitionID, &p.PeriodStart, &p.PeriodEnd, &p.Status, &p.Version, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

const payPeriodCols = `id::text, organization_id::text, definition_id::text, period_start, period_end, status, version, created_at, updated_at`

func scanSchedule(r rowScanner) (Schedule, error) {
	var s Schedule
	err := r.Scan(&s.ID, &s.OrganizationID, &s.EmployeeID, &s.Kind, &s.ExpectedDailyHours, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

const scheduleCols = `id::text, organization_id::text, employee_id::text, kind, expected_daily_hours, version, created_at, updated_at`

func scanShift(r rowScanner) (Shift, error) {
	var s Shift
	err := r.Scan(&s.ID, &s.OrganizationID, &s.EmployeeID, &s.ShiftDate, &s.StartsAt, &s.EndsAt, &s.Published, &s.Version, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

const shiftCols = `id::text, organization_id::text, employee_id::text, shift_date, starts_at, ends_at, published, version, created_at, updated_at`

// periodFrozenAtTx reports whether any pay period covering t is in a frozen
// status (Submitted/Approved).
func periodFrozenAtTx(ctx context.Context, tx pgx.Tx, orgID string, t time.Time) (bool, error) {
	var frozen bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM pay_periods
			WHERE organization_id = $1::uuid
			  AND status IN ('Submitted', 'Approved')
			  AND period_start <= $2 AND period_end > $2)`, orgID, t).Scan(&frozen)
	return frozen, err
}

// latestEntryTx returns the employee's latest head entry (not superseded,
// not deleted), or pgx.ErrNoRows.
func latestEntryTx(ctx context.Context, tx pgx.Tx, orgID, employeeID string) (TimeEntry, error) {
	return scanTimeEntry(tx.QueryRow(ctx, `
		SELECT `+timeEntryCols+` FROM time_entries
		WHERE organization_id = $1::uuid AND employee_id = $2::uuid
		  AND deleted_at IS NULL AND next_id IS NULL
		ORDER BY occurred_at DESC, created_at DESC, id DESC
		LIMIT 1`, orgID, employeeID))
}

// clockLock serializes clock operations per employee inside the transaction,
// mirroring the audit chain's advisory-lock pattern.
func clockLock(ctx context.Context, tx pgx.Tx, employeeID string) error {
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext('forge-hris:clock:' || $1::text))`,
		employeeID)
	return err
}

// employeeExistsTx checks the employee belongs to the org.
func employeeExistsTx(ctx context.Context, tx pgx.Tx, orgID, employeeID string) error {
	var ok bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid)`,
		employeeID, orgID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
	}
	return nil
}

// insertTimeEntryTx validates the period gate and inserts the row.
func insertTimeEntryTx(ctx context.Context, tx pgx.Tx, e TimeEntry) (TimeEntry, error) {
	if err := e.Validate(); err != nil {
		return TimeEntry{}, err
	}
	if e.ID == "" {
		e.ID = newID()
	}
	if e.ClientType == "" {
		e.ClientType = TimeEntryClientWeb
	}
	e.OccurredAt = e.OccurredAt.UTC()
	frozen, err := periodFrozenAtTx(ctx, tx, e.OrganizationID, e.OccurredAt)
	if err != nil {
		return TimeEntry{}, err
	}
	if frozen {
		return TimeEntry{}, fmt.Errorf("%w: entry falls in a frozen pay period", ErrPeriodLocked)
	}
	out, err := scanTimeEntry(tx.QueryRow(ctx, `
		INSERT INTO time_entries
			(id, organization_id, employee_id, kind, occurred_at,
			 project_id, activity_id, location_id, latitude, longitude, address,
			 client_type, prev_id, next_id)
		VALUES
			($1::uuid, $2::uuid, $3::uuid, $4, $5,
			 $6::uuid, $7::uuid, $8::uuid, $9, $10, $11, $12, $13::uuid, $14::uuid)
		RETURNING `+timeEntryCols,
		e.ID, e.OrganizationID, e.EmployeeID, string(e.Kind), e.OccurredAt,
		e.ProjectID, e.ActivityID, e.LocationID, e.Latitude, e.Longitude, e.Address,
		e.ClientType, e.PrevID, e.NextID))
	if err != nil {
		return TimeEntry{}, mapPgError(err, "time entry")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Projects
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateProject(ctx context.Context, p Project) (Project, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	if err := p.Validate(); err != nil {
		return Project{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	var out Project
	err := s.inTenant(ctx, p.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProject(tx.QueryRow(ctx, `
			INSERT INTO projects (id, organization_id, name, client_name)
			VALUES ($1::uuid, $2::uuid, $3, $4)
			RETURNING `+projectCols, p.ID, p.OrganizationID, p.Name, p.ClientName))
		if err != nil {
			return mapPgError(err, "project")
		}
		return nil
	})
	if err != nil {
		return Project{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetProject(ctx context.Context, orgID, id string) (Project, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	var out Project
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProject(tx.QueryRow(ctx,
			`SELECT `+projectCols+` FROM projects WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: project %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return Project{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateProject(ctx context.Context, p Project) (Project, error) {
	if err := ctx.Err(); err != nil {
		return Project{}, err
	}
	if err := p.Validate(); err != nil {
		return Project{}, err
	}
	var out Project
	err := s.inTenant(ctx, p.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanProject(tx.QueryRow(ctx, `
			UPDATE projects
			SET name = $2, client_name = $3, version = version + 1
			WHERE id = $1::uuid AND organization_id = $4::uuid AND version = $5
			RETURNING `+projectCols, p.ID, p.Name, p.ClientName, p.OrganizationID, p.Version))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "projects", p.OrganizationID, p.ID)
			}
			return mapPgError(err, "project")
		}
		return nil
	})
	if err != nil {
		return Project{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteProject(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM projects WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: project %s", ErrNotFound, id)
		}
		return nil
	})
}

func (s *PostgresStore) ListProjects(ctx context.Context, orgID string, pg Page) ([]Project, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(pg.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if pg.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(pg.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []Project
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+projectCols+` FROM projects
			WHERE organization_id = $1::uuid
			  AND (NOT $2 OR (created_at, id) > ($3::timestamptz, NULLIF($4,'')::uuid))
			ORDER BY created_at, id
			LIMIT $5`, orgID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Project, error) { return scanProject(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list projects: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Clock state machine
// ---------------------------------------------------------------------------

// clockTransition runs one clock operation inside a per-employee advisory
// lock: read the latest head entry, apply the transition rule, insert.
func (s *PostgresStore) clockTransition(ctx context.Context, orgID, employeeID string, e TimeEntry,
	rule func(latest *TimeEntry) (TimeEntryKind, error),
) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	var out TimeEntry
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		if err := clockLock(ctx, tx, employeeID); err != nil {
			return err
		}
		if err := employeeExistsTx(ctx, tx, orgID, employeeID); err != nil {
			return err
		}
		latest, err := latestEntryTx(ctx, tx, orgID, employeeID)
		var lp *TimeEntry
		if err == nil {
			lp = &latest
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		kind, err := rule(lp)
		if err != nil {
			return err
		}
		e.OrganizationID = orgID
		e.EmployeeID = employeeID
		e.Kind = kind
		if e.OccurredAt.IsZero() {
			e.OccurredAt = time.Now().UTC()
		}
		out, err = insertTimeEntryTx(ctx, tx, e)
		return err
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) ClockIn(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error) {
	return s.clockTransition(ctx, orgID, employeeID, e, func(latest *TimeEntry) (TimeEntryKind, error) {
		if latest != nil && openKind(latest.Kind) {
			return "", fmt.Errorf("%w: employee %s already clocked in", ErrAlreadyClockedIn, employeeID)
		}
		return TimeEntryIn, nil
	})
}

func (s *PostgresStore) ClockOut(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error) {
	return s.clockTransition(ctx, orgID, employeeID, e, func(latest *TimeEntry) (TimeEntryKind, error) {
		if latest == nil || !openKind(latest.Kind) {
			return "", fmt.Errorf("%w: employee %s has no open entry", ErrNotFound, employeeID)
		}
		if !e.OccurredAt.IsZero() && e.OccurredAt.Before(latest.OccurredAt) {
			return "", &ValidationError{Field: "occurred_at", Message: "must not be before the open entry"}
		}
		return TimeEntryOut, nil
	})
}

func (s *PostgresStore) StartBreak(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error) {
	return s.clockTransition(ctx, orgID, employeeID, e, func(latest *TimeEntry) (TimeEntryKind, error) {
		if latest == nil || !openKind(latest.Kind) {
			return "", fmt.Errorf("%w: employee %s has no open entry", ErrNotFound, employeeID)
		}
		if latest.Kind != TimeEntryIn && latest.Kind != TimeEntryEndBreak {
			return "", fmt.Errorf("%w: cannot start a break while %s", ErrInvalidTransition, latest.Kind)
		}
		return TimeEntryStartBreak, nil
	})
}

func (s *PostgresStore) EndBreak(ctx context.Context, orgID, employeeID string, e TimeEntry) (TimeEntry, error) {
	return s.clockTransition(ctx, orgID, employeeID, e, func(latest *TimeEntry) (TimeEntryKind, error) {
		if latest == nil || latest.Kind != TimeEntryStartBreak {
			return "", fmt.Errorf("%w: employee %s is not on a break", ErrInvalidTransition, employeeID)
		}
		return TimeEntryEndBreak, nil
	})
}

func (s *PostgresStore) OpenTimeEntry(ctx context.Context, orgID, employeeID string) (TimeEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, false, err
	}
	var out TimeEntry
	var open bool
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		latest, err := latestEntryTx(ctx, tx, orgID, employeeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if openKind(latest.Kind) {
			out, open = latest, true
		}
		return nil
	})
	if err != nil {
		return TimeEntry{}, false, err
	}
	return out, open, nil
}

// ---------------------------------------------------------------------------
// Time entries
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateTimeEntry(ctx context.Context, e TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	var out TimeEntry
	err := s.inTenant(ctx, e.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = insertTimeEntryTx(ctx, tx, e)
		return err
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetTimeEntry(ctx context.Context, orgID, id string) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	var out TimeEntry
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanTimeEntry(tx.QueryRow(ctx,
			`SELECT `+timeEntryCols+` FROM time_entries WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: time entry %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return out, nil
}

// mutableEntryTx loads the entry and enforces the mutability gate (not
// deleted, not locked, not inside a frozen period).
func mutableEntryTx(ctx context.Context, tx pgx.Tx, orgID, id string) (TimeEntry, error) {
	e, err := scanTimeEntry(tx.QueryRow(ctx,
		`SELECT `+timeEntryCols+` FROM time_entries
		 WHERE id = $1::uuid AND organization_id = $2::uuid AND deleted_at IS NULL`,
		id, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, id)
	}
	if err != nil {
		return TimeEntry{}, err
	}
	if e.IsLocked {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s is locked", ErrPeriodLocked, id)
	}
	frozen, err := periodFrozenAtTx(ctx, tx, orgID, e.OccurredAt)
	if err != nil {
		return TimeEntry{}, err
	}
	if frozen {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s falls in a frozen pay period", ErrPeriodLocked, id)
	}
	return e, nil
}

func (s *PostgresStore) UpdateTimeEntry(ctx context.Context, e TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	if err := e.Validate(); err != nil {
		return TimeEntry{}, err
	}
	var out TimeEntry
	err := s.inTenant(ctx, e.OrganizationID, func(tx pgx.Tx) error {
		cur, err := mutableEntryTx(ctx, tx, e.OrganizationID, e.ID)
		if err != nil {
			return err
		}
		if e.Kind != cur.Kind {
			return &ValidationError{Field: "kind", Message: "cannot change kind; use CorrectTimeEntry"}
		}
		if e.EmployeeID != cur.EmployeeID {
			return &ValidationError{Field: "employee_id", Message: "cannot reassign entries between employees"}
		}
		frozen, err := periodFrozenAtTx(ctx, tx, e.OrganizationID, e.OccurredAt)
		if err != nil {
			return err
		}
		if frozen {
			return fmt.Errorf("%w: new occurred_at falls in a frozen pay period", ErrPeriodLocked)
		}
		clientType := e.ClientType
		if clientType == "" {
			clientType = TimeEntryClientWeb
		}
		var err2 error
		out, err2 = scanTimeEntry(tx.QueryRow(ctx, `
			UPDATE time_entries
			SET occurred_at = $3, project_id = $4::uuid, activity_id = $5::uuid,
			    location_id = $6::uuid, latitude = $7, longitude = $8, address = $9,
			    client_type = $10, version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $11
			  AND deleted_at IS NULL
			RETURNING `+timeEntryCols,
			e.ID, e.OrganizationID, e.OccurredAt.UTC(), e.ProjectID, e.ActivityID,
			e.LocationID, e.Latitude, e.Longitude, e.Address, clientType, e.Version))
		if err2 != nil {
			if errors.Is(err2, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "time_entries", e.OrganizationID, e.ID)
			}
			return mapPgError(err2, "time entry")
		}
		return nil
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteTimeEntry(ctx context.Context, orgID, id string, version int64) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	var out TimeEntry
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		if _, err := mutableEntryTx(ctx, tx, orgID, id); err != nil {
			return err
		}
		var err2 error
		out, err2 = scanTimeEntry(tx.QueryRow(ctx, `
			UPDATE time_entries
			SET deleted_at = now(), version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $3
			  AND deleted_at IS NULL
			RETURNING `+timeEntryCols, id, orgID, version))
		if err2 != nil {
			if errors.Is(err2, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "time_entries", orgID, id)
			}
			return err2
		}
		return nil
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) CorrectTimeEntry(ctx context.Context, orgID, id string, correction TimeEntry) (TimeEntry, error) {
	if err := ctx.Err(); err != nil {
		return TimeEntry{}, err
	}
	var out TimeEntry
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		// Precedence: "already corrected" is the more specific error for a
		// frozen-via-correction original, so check the chain before the lock.
		var nextID *string
		err := tx.QueryRow(ctx,
			`SELECT next_id::text FROM time_entries
			 WHERE id = $1::uuid AND organization_id = $2::uuid AND deleted_at IS NULL`,
			id, orgID).Scan(&nextID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: time entry %s", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if nextID != nil {
			return fmt.Errorf("%w: time entry %s already corrected", ErrInvalidTransition, id)
		}
		orig, err := mutableEntryTx(ctx, tx, orgID, id)
		if err != nil {
			return err
		}
		correction.OrganizationID = orgID
		correction.EmployeeID = orig.EmployeeID
		if correction.OccurredAt.IsZero() {
			correction.OccurredAt = orig.OccurredAt
		}
		correction.PrevID = &orig.ID
		created, err := insertTimeEntryTx(ctx, tx, correction)
		if err != nil {
			return err
		}
		// Freeze the original and link it forward to the correction.
		_, err = tx.Exec(ctx, `
			UPDATE time_entries
			SET next_id = $3::uuid, is_locked = true, version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid AND next_id IS NULL`,
			id, orgID, created.ID)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListTimeEntries(ctx context.Context, orgID string, f TimeEntryFilter, pg Page) ([]TimeEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(pg.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if pg.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(pg.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []TimeEntry
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+timeEntryCols+` FROM time_entries
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR employee_id = $2::uuid)
			  AND ($3 = '' OR kind = $3)
			  AND ($4::timestamptz IS NULL OR occurred_at >= $4::timestamptz)
			  AND ($5::timestamptz IS NULL OR occurred_at < $5::timestamptz)
			  AND (deleted_at IS NULL OR $6)
			  AND (next_id IS NULL OR $7)
			  AND (NOT $8 OR (created_at, id) > ($9::timestamptz, NULLIF($10,'')::uuid))
			ORDER BY created_at, id
			LIMIT $11`,
			orgID, f.EmployeeID, string(f.Kind), f.From, f.To,
			f.IncludeDeleted, f.IncludeHistory, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimeEntry, error) { return scanTimeEntry(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list time entries: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Hour entries
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateHourEntry(ctx context.Context, e HourEntry) (HourEntry, error) {
	if err := ctx.Err(); err != nil {
		return HourEntry{}, err
	}
	if err := e.Validate(); err != nil {
		return HourEntry{}, err
	}
	if e.ID == "" {
		e.ID = newID()
	}
	e.WorkDate = e.WorkDate.UTC()
	var out HourEntry
	err := s.inTenant(ctx, e.OrganizationID, func(tx pgx.Tx) error {
		frozen, err := periodFrozenAtTx(ctx, tx, e.OrganizationID, e.WorkDate)
		if err != nil {
			return err
		}
		if frozen {
			return fmt.Errorf("%w: work date falls in a frozen pay period", ErrPeriodLocked)
		}
		var err2 error
		out, err2 = scanHourEntry(tx.QueryRow(ctx, `
			INSERT INTO hour_entries
				(id, organization_id, employee_id, work_date, hours, project_id, note)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6::uuid, $7)
			RETURNING `+hourEntryCols,
			e.ID, e.OrganizationID, e.EmployeeID, e.WorkDate, e.Hours, e.ProjectID, e.Note))
		if err2 != nil {
			return mapPgError(err2, "hour entry")
		}
		return nil
	})
	if err != nil {
		return HourEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetHourEntry(ctx context.Context, orgID, id string) (HourEntry, error) {
	if err := ctx.Err(); err != nil {
		return HourEntry{}, err
	}
	var out HourEntry
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanHourEntry(tx.QueryRow(ctx,
			`SELECT `+hourEntryCols+` FROM hour_entries WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: hour entry %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return HourEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateHourEntry(ctx context.Context, e HourEntry) (HourEntry, error) {
	if err := ctx.Err(); err != nil {
		return HourEntry{}, err
	}
	if err := e.Validate(); err != nil {
		return HourEntry{}, err
	}
	var out HourEntry
	err := s.inTenant(ctx, e.OrganizationID, func(tx pgx.Tx) error {
		var curWorkDate time.Time
		var curEmployee string
		err := tx.QueryRow(ctx,
			`SELECT work_date, employee_id::text FROM hour_entries
			 WHERE id = $1::uuid AND organization_id = $2::uuid AND deleted_at IS NULL`,
			e.ID, e.OrganizationID).Scan(&curWorkDate, &curEmployee)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: hour entry %s", ErrNotFound, e.ID)
		}
		if err != nil {
			return err
		}
		if e.EmployeeID != curEmployee {
			return &ValidationError{Field: "employee_id", Message: "cannot reassign entries between employees"}
		}
		for _, d := range []time.Time{curWorkDate, e.WorkDate} {
			frozen, err := periodFrozenAtTx(ctx, tx, e.OrganizationID, d)
			if err != nil {
				return err
			}
			if frozen {
				return fmt.Errorf("%w: hour entry falls in a frozen pay period", ErrPeriodLocked)
			}
		}
		var err2 error
		out, err2 = scanHourEntry(tx.QueryRow(ctx, `
			UPDATE hour_entries
			SET work_date = $3, hours = $4, project_id = $5::uuid, note = $6,
			    version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $7
			  AND deleted_at IS NULL
			RETURNING `+hourEntryCols,
			e.ID, e.OrganizationID, e.WorkDate.UTC(), e.Hours, e.ProjectID, e.Note, e.Version))
		if err2 != nil {
			if errors.Is(err2, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "hour_entries", e.OrganizationID, e.ID)
			}
			return mapPgError(err2, "hour entry")
		}
		return nil
	})
	if err != nil {
		return HourEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteHourEntry(ctx context.Context, orgID, id string, version int64) (HourEntry, error) {
	if err := ctx.Err(); err != nil {
		return HourEntry{}, err
	}
	var out HourEntry
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var workDate time.Time
		err := tx.QueryRow(ctx,
			`SELECT work_date FROM hour_entries
			 WHERE id = $1::uuid AND organization_id = $2::uuid AND deleted_at IS NULL`,
			id, orgID).Scan(&workDate)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: hour entry %s", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		frozen, err := periodFrozenAtTx(ctx, tx, orgID, workDate)
		if err != nil {
			return err
		}
		if frozen {
			return fmt.Errorf("%w: hour entry falls in a frozen pay period", ErrPeriodLocked)
		}
		var err2 error
		out, err2 = scanHourEntry(tx.QueryRow(ctx, `
			UPDATE hour_entries
			SET deleted_at = now(), version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $3
			  AND deleted_at IS NULL
			RETURNING `+hourEntryCols, id, orgID, version))
		if err2 != nil {
			if errors.Is(err2, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "hour_entries", orgID, id)
			}
			return err2
		}
		return nil
	})
	if err != nil {
		return HourEntry{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListHourEntries(ctx context.Context, orgID string, f HourEntryFilter, pg Page) ([]HourEntry, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(pg.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if pg.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(pg.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []HourEntry
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+hourEntryCols+` FROM hour_entries
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR employee_id = $2::uuid)
			  AND ($3::timestamptz IS NULL OR work_date >= $3::timestamptz)
			  AND ($4::timestamptz IS NULL OR work_date < $4::timestamptz)
			  AND (deleted_at IS NULL OR $5)
			  AND (NOT $6 OR (created_at, id) > ($7::timestamptz, NULLIF($8,'')::uuid))
			ORDER BY created_at, id
			LIMIT $9`,
			orgID, f.EmployeeID, f.From, f.To, f.IncludeDeleted,
			haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (HourEntry, error) { return scanHourEntry(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list hour entries: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Pay-period definitions
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreatePayPeriodDefinition(ctx context.Context, d PayPeriodDefinition) (PayPeriodDefinition, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriodDefinition{}, err
	}
	if err := d.Validate(); err != nil {
		return PayPeriodDefinition{}, err
	}
	if d.ID == "" {
		d.ID = newID()
	}
	var out PayPeriodDefinition
	err := s.inTenant(ctx, d.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayPeriodDefinition(tx.QueryRow(ctx, `
			INSERT INTO pay_period_definitions (id, organization_id, name, cadence, anchor_day)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5)
			RETURNING `+payPeriodDefinitionCols, d.ID, d.OrganizationID, d.Name, string(d.Cadence), d.AnchorDay))
		if err != nil {
			return mapPgError(err, "pay period definition")
		}
		return nil
	})
	if err != nil {
		return PayPeriodDefinition{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetPayPeriodDefinition(ctx context.Context, orgID, id string) (PayPeriodDefinition, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriodDefinition{}, err
	}
	var out PayPeriodDefinition
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayPeriodDefinition(tx.QueryRow(ctx,
			`SELECT `+payPeriodDefinitionCols+` FROM pay_period_definitions WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: pay period definition %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return PayPeriodDefinition{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdatePayPeriodDefinition(ctx context.Context, d PayPeriodDefinition) (PayPeriodDefinition, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriodDefinition{}, err
	}
	if err := d.Validate(); err != nil {
		return PayPeriodDefinition{}, err
	}
	var out PayPeriodDefinition
	err := s.inTenant(ctx, d.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayPeriodDefinition(tx.QueryRow(ctx, `
			UPDATE pay_period_definitions
			SET name = $2, cadence = $3, anchor_day = $4, version = version + 1
			WHERE id = $1::uuid AND organization_id = $5::uuid AND version = $6
			RETURNING `+payPeriodDefinitionCols,
			d.ID, d.Name, string(d.Cadence), d.AnchorDay, d.OrganizationID, d.Version))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "pay_period_definitions", d.OrganizationID, d.ID)
			}
			return mapPgError(err, "pay period definition")
		}
		return nil
	})
	if err != nil {
		return PayPeriodDefinition{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeletePayPeriodDefinition(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM pay_periods WHERE definition_id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: definition %s still has pay periods", ErrNotEmpty, id)
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM pay_period_definitions WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: pay period definition %s", ErrNotFound, id)
		}
		return nil
	})
}

func (s *PostgresStore) ListPayPeriodDefinitions(ctx context.Context, orgID string, pg Page) ([]PayPeriodDefinition, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(pg.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if pg.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(pg.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []PayPeriodDefinition
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+payPeriodDefinitionCols+` FROM pay_period_definitions
			WHERE organization_id = $1::uuid
			  AND (NOT $2 OR (created_at, id) > ($3::timestamptz, NULLIF($4,'')::uuid))
			ORDER BY created_at, id
			LIMIT $5`, orgID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayPeriodDefinition, error) {
			return scanPayPeriodDefinition(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list pay period definitions: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Pay periods
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreatePayPeriod(ctx context.Context, orgID, definitionID string, periodStart time.Time) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	var out PayPeriod
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		def, err := scanPayPeriodDefinition(tx.QueryRow(ctx,
			`SELECT `+payPeriodDefinitionCols+` FROM pay_period_definitions WHERE id = $1::uuid AND organization_id = $2::uuid`,
			definitionID, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: pay period definition %s", ErrNotFound, definitionID)
		}
		if err != nil {
			return err
		}
		start, end, err := ComputePayPeriodRange(def, periodStart)
		if err != nil {
			return err
		}
		var err2 error
		out, err2 = scanPayPeriod(tx.QueryRow(ctx, `
			INSERT INTO pay_periods (organization_id, definition_id, period_start, period_end, status)
			VALUES ($1::uuid, $2::uuid, $3, $4, 'Open')
			RETURNING `+payPeriodCols, orgID, definitionID, start, end))
		if err2 != nil {
			return mapPgError(err2, "pay period")
		}
		return nil
	})
	if err != nil {
		return PayPeriod{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetPayPeriod(ctx context.Context, orgID, id string) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	var out PayPeriod
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayPeriod(tx.QueryRow(ctx,
			`SELECT `+payPeriodCols+` FROM pay_periods WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: pay period %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return PayPeriod{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListPayPeriods(ctx context.Context, orgID string, f PayPeriodFilter, pg Page) ([]PayPeriod, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(pg.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if pg.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(pg.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []PayPeriod
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+payPeriodCols+` FROM pay_periods
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR definition_id = $2::uuid)
			  AND ($3 = '' OR status = $3)
			  AND (NOT $4 OR (created_at, id) > ($5::timestamptz, NULLIF($6,'')::uuid))
			ORDER BY created_at, id
			LIMIT $7`, orgID, f.DefinitionID, string(f.Status), haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayPeriod, error) { return scanPayPeriod(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list pay periods: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// transitionPayPeriodTx applies a version-checked status transition inside
// the transaction; approval locks the covered entries, reopening unlocks.
func transitionPayPeriodTx(ctx context.Context, tx pgx.Tx, orgID, id string, version int64, to PayPeriodStatus) (PayPeriod, error) {
	cur, err := scanPayPeriod(tx.QueryRow(ctx,
		`SELECT `+payPeriodCols+` FROM pay_periods WHERE id = $1::uuid AND organization_id = $2::uuid`,
		id, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PayPeriod{}, fmt.Errorf("%w: pay period %s", ErrNotFound, id)
	}
	if err != nil {
		return PayPeriod{}, err
	}
	if version != cur.Version {
		return PayPeriod{}, fmt.Errorf("%w: pay period %s", ErrVersionConflict, id)
	}
	if !validPayPeriodTransition(cur.Status, to) {
		return PayPeriod{}, fmt.Errorf("%w: cannot move pay period from %s to %s", ErrInvalidTransition, cur.Status, to)
	}
	out, err := scanPayPeriod(tx.QueryRow(ctx, `
		UPDATE pay_periods
		SET status = $3, version = version + 1
		WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $4
		RETURNING `+payPeriodCols, id, orgID, string(to), version))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PayPeriod{}, fmt.Errorf("%w: pay period %s", ErrVersionConflict, id)
		}
		return PayPeriod{}, err
	}
	if to == PayPeriodApproved || to == PayPeriodReopened {
		locked := to == PayPeriodApproved
		if _, err := tx.Exec(ctx, `
			UPDATE time_entries
			SET is_locked = $4, version = version + 1
			WHERE organization_id = $1::uuid
			  AND occurred_at >= $2 AND occurred_at < $3
			  AND deleted_at IS NULL`,
			orgID, out.PeriodStart, out.PeriodEnd, locked); err != nil {
			return PayPeriod{}, err
		}
	}
	return out, nil
}

func (s *PostgresStore) SubmitPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	var out PayPeriod
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = transitionPayPeriodTx(ctx, tx, orgID, id, version, PayPeriodSubmitted)
		return err
	})
	if err != nil {
		return PayPeriod{}, err
	}
	return out, nil
}

func (s *PostgresStore) ApprovePayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	var out PayPeriod
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = transitionPayPeriodTx(ctx, tx, orgID, id, version, PayPeriodApproved)
		return err
	})
	if err != nil {
		return PayPeriod{}, err
	}
	return out, nil
}

func (s *PostgresStore) RejectPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	var out PayPeriod
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = transitionPayPeriodTx(ctx, tx, orgID, id, version, PayPeriodRejected)
		return err
	})
	if err != nil {
		return PayPeriod{}, err
	}
	return out, nil
}

func (s *PostgresStore) ReopenPayPeriod(ctx context.Context, orgID, id string, version int64) (PayPeriod, error) {
	if err := ctx.Err(); err != nil {
		return PayPeriod{}, err
	}
	var out PayPeriod
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = transitionPayPeriodTx(ctx, tx, orgID, id, version, PayPeriodReopened)
		return err
	})
	if err != nil {
		return PayPeriod{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Schedules
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	if err := sc.Validate(); err != nil {
		return Schedule{}, err
	}
	if sc.ID == "" {
		sc.ID = newID()
	}
	var out Schedule
	err := s.inTenant(ctx, sc.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanSchedule(tx.QueryRow(ctx, `
			INSERT INTO schedules (id, organization_id, employee_id, kind, expected_daily_hours)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5)
			RETURNING `+scheduleCols,
			sc.ID, sc.OrganizationID, sc.EmployeeID, string(sc.Kind), sc.ExpectedDailyHours))
		if err != nil {
			return mapPgError(err, "schedule")
		}
		return nil
	})
	if err != nil {
		return Schedule{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetSchedule(ctx context.Context, orgID, id string) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	var out Schedule
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanSchedule(tx.QueryRow(ctx,
			`SELECT `+scheduleCols+` FROM schedules WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: schedule %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return Schedule{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateSchedule(ctx context.Context, sc Schedule) (Schedule, error) {
	if err := ctx.Err(); err != nil {
		return Schedule{}, err
	}
	if err := sc.Validate(); err != nil {
		return Schedule{}, err
	}
	var out Schedule
	err := s.inTenant(ctx, sc.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanSchedule(tx.QueryRow(ctx, `
			UPDATE schedules
			SET employee_id = $2::uuid, kind = $3, expected_daily_hours = $4,
			    version = version + 1
			WHERE id = $1::uuid AND organization_id = $5::uuid AND version = $6
			RETURNING `+scheduleCols,
			sc.ID, sc.EmployeeID, string(sc.Kind), sc.ExpectedDailyHours, sc.OrganizationID, sc.Version))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "schedules", sc.OrganizationID, sc.ID)
			}
			return mapPgError(err, "schedule")
		}
		return nil
	})
	if err != nil {
		return Schedule{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteSchedule(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM schedules WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: schedule %s", ErrNotFound, id)
		}
		return nil
	})
}

func (s *PostgresStore) ListSchedules(ctx context.Context, orgID, employeeID string, pg Page) ([]Schedule, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(pg.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if pg.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(pg.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []Schedule
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+scheduleCols+` FROM schedules
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR employee_id = $2::uuid)
			  AND (NOT $3 OR (created_at, id) > ($4::timestamptz, NULLIF($5,'')::uuid))
			ORDER BY created_at, id
			LIMIT $6`, orgID, employeeID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Schedule, error) { return scanSchedule(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list schedules: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Shifts
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateShift(ctx context.Context, sh Shift) (Shift, error) {
	if err := ctx.Err(); err != nil {
		return Shift{}, err
	}
	if err := sh.Validate(); err != nil {
		return Shift{}, err
	}
	if sh.ID == "" {
		sh.ID = newID()
	}
	sh.ShiftDate = sh.ShiftDate.UTC()
	var out Shift
	err := s.inTenant(ctx, sh.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanShift(tx.QueryRow(ctx, `
			INSERT INTO shifts (id, organization_id, employee_id, shift_date, starts_at, ends_at, published)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6, $7)
			RETURNING `+shiftCols,
			sh.ID, sh.OrganizationID, sh.EmployeeID, sh.ShiftDate, sh.StartsAt, sh.EndsAt, sh.Published))
		if err != nil {
			return mapPgError(err, "shift")
		}
		return nil
	})
	if err != nil {
		return Shift{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetShift(ctx context.Context, orgID, id string) (Shift, error) {
	if err := ctx.Err(); err != nil {
		return Shift{}, err
	}
	var out Shift
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanShift(tx.QueryRow(ctx,
			`SELECT `+shiftCols+` FROM shifts WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: shift %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return Shift{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateShift(ctx context.Context, sh Shift) (Shift, error) {
	if err := ctx.Err(); err != nil {
		return Shift{}, err
	}
	if err := sh.Validate(); err != nil {
		return Shift{}, err
	}
	var out Shift
	err := s.inTenant(ctx, sh.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanShift(tx.QueryRow(ctx, `
			UPDATE shifts
			SET employee_id = $2::uuid, shift_date = $3, starts_at = $4, ends_at = $5,
			    published = $6, version = version + 1
			WHERE id = $1::uuid AND organization_id = $7::uuid AND version = $8
			RETURNING `+shiftCols,
			sh.ID, sh.EmployeeID, sh.ShiftDate.UTC(), sh.StartsAt, sh.EndsAt,
			sh.Published, sh.OrganizationID, sh.Version))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return versionOutcome(ctx, tx, "shifts", sh.OrganizationID, sh.ID)
			}
			return mapPgError(err, "shift")
		}
		return nil
	})
	if err != nil {
		return Shift{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteShift(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM shifts WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("%w: shift %s", ErrNotFound, id)
		}
		return nil
	})
}

func (s *PostgresStore) ListShifts(ctx context.Context, orgID string, f ShiftFilter, pg Page) ([]Shift, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(pg.Limit)
	var ct time.Time
	var cid string
	haveCursor := false
	if pg.Cursor != "" {
		var err error
		ct, cid, err = decodeCursor(pg.Cursor)
		if err != nil {
			return nil, "", err
		}
		haveCursor = true
	}
	var out []Shift
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+shiftCols+` FROM shifts
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR employee_id = $2::uuid)
			  AND ($3::timestamptz IS NULL OR shift_date >= $3::timestamptz)
			  AND ($4::timestamptz IS NULL OR shift_date < $4::timestamptz)
			  AND ($5::boolean IS NULL OR published = $5::boolean)
			  AND (NOT $6 OR (created_at, id) > ($7::timestamptz, NULLIF($8,'')::uuid))
			ORDER BY created_at, id
			LIMIT $9`,
			orgID, f.EmployeeID, f.From, f.To, f.Published,
			haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Shift, error) { return scanShift(r) })
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list shifts: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *PostgresStore) PublishShifts(ctx context.Context, orgID string, ids []string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	var uniq []string
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	var n int64
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM shifts WHERE organization_id = $1::uuid AND id = ANY($2::uuid[])`,
			orgID, uniq).Scan(&count); err != nil {
			return err
		}
		if count != len(uniq) {
			return fmt.Errorf("%w: one or more shifts", ErrNotFound)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE shifts
			SET published = true, version = version + 1
			WHERE organization_id = $1::uuid AND id = ANY($2::uuid[]) AND published = false`,
			orgID, uniq)
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
