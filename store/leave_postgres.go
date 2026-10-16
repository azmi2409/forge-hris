package store

// PostgresStore leave implementation (component 3). Every org-scoped
// operation runs inside s.inTenant (app.tenant_id + explicit
// organization_id predicates); version-checked writes map 0-row outcomes via
// versionOutcome; constraint violations via mapPgError.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Compile-time contract check (Worker C embeds LeaveStore into Store).
var _ LeaveStore = (*PostgresStore)(nil)

// ---------------------------------------------------------------------------
// Row scanners. UUID columns are selected as ::text so they scan straight
// into Go strings; DATE columns scan into time.Time and are formatted to
// YYYY-MM-DD.
// ---------------------------------------------------------------------------

func scanTimeOffPolicy(r rowScanner) (TimeOffPolicy, error) {
	var p TimeOffPolicy
	err := r.Scan(&p.ID, &p.OrganizationID, &p.Name, &p.AccrualMode,
		&p.AccrualAmount, &p.CarryForwardCap, &p.ExpiryMonths,
		&p.Version, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

const timeOffPolicyCols = `id::text, organization_id::text, name, accrual_mode,
	accrual_amount, carry_forward_cap, expiry_months, version, created_at, updated_at`

func scanLeaveBalance(r rowScanner) (LeaveBalance, error) {
	var b LeaveBalance
	err := r.Scan(&b.ID, &b.OrganizationID, &b.EmployeeID, &b.PolicyID, &b.Period,
		&b.Accrued, &b.Taken, &b.Carried, &b.Expired, &b.Balance, &b.ComputedAt,
		&b.Version, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

const leaveBalanceCols = `id::text, organization_id::text, employee_id::text, policy_id::text,
	period, accrued, taken, carried, expired, balance, computed_at,
	version, created_at, updated_at`

func scanTimeOffRequest(r rowScanner) (TimeOffRequest, error) {
	var q TimeOffRequest
	var start, end time.Time
	var snap []byte
	err := r.Scan(&q.ID, &q.OrganizationID, &q.EmployeeID, &q.PolicyID,
		&q.HolidayCalendarID, &start, &end, &q.Days, &snap,
		&q.Status, &q.IsUnpaid, &q.Version, &q.CreatedAt, &q.UpdatedAt)
	if err != nil {
		return TimeOffRequest{}, err
	}
	q.StartDate = start.Format(dateLayout)
	q.EndDate = end.Format(dateLayout)
	q.Snapshot = json.RawMessage(snap)
	return q, nil
}

const timeOffRequestCols = `id::text, organization_id::text, employee_id::text, policy_id::text,
	holiday_calendar_id::text, start_date, end_date, days, snapshot, status, is_unpaid,
	version, created_at, updated_at`

func scanTimeOffApproval(r rowScanner) (TimeOffApproval, error) {
	var a TimeOffApproval
	err := r.Scan(&a.ID, &a.OrganizationID, &a.RequestID, &a.Sequence,
		&a.ApproverEmployeeID, &a.Status, &a.DecidedBy, &a.DecidedAt, &a.CreatedAt)
	return a, err
}

const timeOffApprovalCols = `id::text, organization_id::text, request_id::text, sequence,
	approver_employee_id::text, status, decided_by, decided_at, created_at`

func scanHolidayCalendar(r rowScanner) (HolidayCalendar, error) {
	var c HolidayCalendar
	err := r.Scan(&c.ID, &c.OrganizationID, &c.Name, &c.CountryCode,
		&c.Version, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

const holidayCalendarCols = `id::text, organization_id::text, name, country_code,
	version, created_at, updated_at`

func scanHoliday(r rowScanner) (Holiday, error) {
	var h Holiday
	var d time.Time
	err := r.Scan(&h.ID, &h.OrganizationID, &h.CalendarID, &d, &h.Name, &h.CreatedAt)
	if err != nil {
		return Holiday{}, err
	}
	h.Date = d.Format(dateLayout)
	return h, nil
}

const holidayCols = `id::text, organization_id::text, calendar_id::text,
	date, name, created_at`

// ---------------------------------------------------------------------------
// Shared tx helpers.
// ---------------------------------------------------------------------------

// policyTx loads a policy row inside a tenant transaction.
func policyTx(ctx context.Context, tx pgx.Tx, orgID, id string) (TimeOffPolicy, error) {
	p, err := scanTimeOffPolicy(tx.QueryRow(ctx,
		`SELECT `+timeOffPolicyCols+` FROM time_off_policies WHERE id = $1::uuid AND organization_id = $2::uuid`,
		id, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return TimeOffPolicy{}, fmt.Errorf("%w: time-off policy %s", ErrNotFound, id)
	}
	return p, err
}

// employeeTx loads an employee row inside a tenant transaction.
func employeeTx(ctx context.Context, tx pgx.Tx, orgID, id string) (Employee, error) {
	e, err := scanEmployee(tx.QueryRow(ctx,
		`SELECT `+employeeCols+` FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid`,
		id, orgID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Employee{}, fmt.Errorf("%w: employee %s", ErrNotFound, id)
	}
	return e, err
}

// requestsTx lists an employee's requests for a policy inside a tenant
// transaction, excluding exceptID.
func requestsTx(ctx context.Context, tx pgx.Tx, orgID, employeeID, policyID, exceptID string) ([]TimeOffRequest, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+timeOffRequestCols+` FROM time_off_requests
		WHERE organization_id = $1::uuid AND employee_id = $2::uuid AND policy_id = $3::uuid
		  AND ($4 = '' OR id != $4::uuid)
		ORDER BY created_at, id`, orgID, employeeID, policyID, exceptID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimeOffRequest, error) {
		return scanTimeOffRequest(r)
	})
}

// computeBalanceTx recomputes the full balance inside a tenant transaction
// (pure recalc + carry-forward from the stored previous-period balance +
// expiry). It also returns the unfloored pool value for sufficiency checks.
func computeBalanceTx(ctx context.Context, tx pgx.Tx, orgID string, emp Employee, pol TimeOffPolicy, holidays []Holiday, asOf time.Time, exceptRequestID string) (LeaveBalance, float64, error) {
	period := asOf.UTC().Format("2006")
	var prev *LeaveBalance
	p, err := scanLeaveBalance(tx.QueryRow(ctx, `
		SELECT `+leaveBalanceCols+` FROM leave_balances
		WHERE organization_id = $1::uuid AND employee_id = $2::uuid
		  AND policy_id = $3::uuid AND period = $4`,
		orgID, emp.ID, pol.ID, prevPeriod(period)))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return LeaveBalance{}, 0, err
	}
	if err == nil {
		prev = &p
	}
	requests, err := requestsTx(ctx, tx, orgID, emp.ID, pol.ID, exceptRequestID)
	if err != nil {
		return LeaveBalance{}, 0, err
	}
	bal := RecalculateBalance(pol, emp, requests, holidays, asOf)
	fb, raw := finalizeBalance(bal, prev, pol, asOf)
	return fb, raw, nil
}

// validateApproverTx checks the approver is a joined employee in the org.
func validateApproverTx(ctx context.Context, tx pgx.Tx, orgID, approverID string) error {
	var status string
	err := tx.QueryRow(ctx,
		`SELECT status FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid`,
		approverID, orgID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ValidationError{Field: "approver_ids", Message: "unknown employee " + approverID}
	}
	if err != nil {
		return err
	}
	if InvitationStatus(status) != InvitationJoined {
		return &ValidationError{Field: "approver_ids", Message: "employee " + approverID + " is not joined"}
	}
	return nil
}

// dedupeApprovers drops blanks and duplicates, preserving order.
func dedupeApprovers(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// snapshotJSON freezes the policy rules and the balance at request time.
func snapshotJSON(pol TimeOffPolicy, bal LeaveBalance) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"policy": map[string]any{
			"id":                pol.ID,
			"name":              pol.Name,
			"accrual_mode":      string(pol.AccrualMode),
			"accrual_amount":    pol.AccrualAmount,
			"carry_forward_cap": pol.CarryForwardCap,
			"expiry_months":     pol.ExpiryMonths,
			"version":           pol.Version,
		},
		"balance_at_request": map[string]any{
			"period":   bal.Period,
			"accrued":  bal.Accrued,
			"taken":    bal.Taken,
			"carried":  bal.Carried,
			"expired":  bal.Expired,
			"balance":  bal.Balance,
			"computed": bal.ComputedAt,
		},
	})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// ---------------------------------------------------------------------------
// Time-off policies
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateTimeOffPolicy(ctx context.Context, p TimeOffPolicy) (TimeOffPolicy, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffPolicy{}, err
	}
	if err := p.Validate(); err != nil {
		return TimeOffPolicy{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	var out TimeOffPolicy
	err := s.inTenant(ctx, p.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanTimeOffPolicy(tx.QueryRow(ctx, `
			INSERT INTO time_off_policies (id, organization_id, name, accrual_mode, accrual_amount, carry_forward_cap, expiry_months)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)
			RETURNING `+timeOffPolicyCols,
			p.ID, p.OrganizationID, p.Name, string(p.AccrualMode), p.AccrualAmount, p.CarryForwardCap, p.ExpiryMonths))
		return mapPgError(err, "time-off policy")
	})
	if err != nil {
		return TimeOffPolicy{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetTimeOffPolicy(ctx context.Context, orgID, id string) (TimeOffPolicy, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffPolicy{}, err
	}
	var out TimeOffPolicy
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = policyTx(ctx, tx, orgID, id)
		return err
	})
	if err != nil {
		return TimeOffPolicy{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateTimeOffPolicy(ctx context.Context, p TimeOffPolicy) (TimeOffPolicy, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffPolicy{}, err
	}
	if err := p.Validate(); err != nil {
		return TimeOffPolicy{}, err
	}
	var out TimeOffPolicy
	err := s.inTenant(ctx, p.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanTimeOffPolicy(tx.QueryRow(ctx, `
			UPDATE time_off_policies
			SET name = $2, accrual_mode = $3, accrual_amount = $4,
			    carry_forward_cap = $5, expiry_months = $6, version = version + 1
			WHERE id = $1::uuid AND organization_id = $7::uuid AND version = $8
			RETURNING `+timeOffPolicyCols,
			p.ID, p.Name, string(p.AccrualMode), p.AccrualAmount, p.CarryForwardCap,
			p.ExpiryMonths, p.OrganizationID, p.Version))
		if errors.Is(err, pgx.ErrNoRows) {
			return versionOutcome(ctx, tx, "time_off_policies", p.OrganizationID, p.ID)
		}
		return mapPgError(err, "time-off policy")
	})
	if err != nil {
		return TimeOffPolicy{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteTimeOffPolicy(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM time_off_policies WHERE id = $1::uuid AND organization_id = $2::uuid)`,
			id, orgID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: time-off policy %s", ErrNotFound, id)
		}
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM time_off_requests WHERE policy_id = $1::uuid AND organization_id = $2::uuid)`,
			id, orgID).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%w: policy %s still has leave requests", ErrNotEmpty, id)
		}
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM leave_balances WHERE policy_id = $1::uuid AND organization_id = $2::uuid)`,
			id, orgID).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%w: policy %s still has leave balances", ErrNotEmpty, id)
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM time_off_policies WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		return err
	})
}

func (s *PostgresStore) ListTimeOffPolicies(ctx context.Context, orgID string, p Page) ([]TimeOffPolicy, string, error) {
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
	var out []TimeOffPolicy
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+timeOffPolicyCols+` FROM time_off_policies
			WHERE organization_id = $1::uuid
			  AND (NOT $2 OR (created_at, id) > ($3::timestamptz, NULLIF($4,'')::uuid))
			ORDER BY created_at, id
			LIMIT $5`, orgID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimeOffPolicy, error) {
			return scanTimeOffPolicy(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list time-off policies: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Leave balances
// ---------------------------------------------------------------------------

func (s *PostgresStore) GetLeaveBalance(ctx context.Context, orgID, employeeID, policyID, period string) (LeaveBalance, error) {
	if err := ctx.Err(); err != nil {
		return LeaveBalance{}, err
	}
	var out LeaveBalance
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanLeaveBalance(tx.QueryRow(ctx, `
			SELECT `+leaveBalanceCols+` FROM leave_balances
			WHERE organization_id = $1::uuid AND employee_id = $2::uuid
			  AND policy_id = $3::uuid AND period = $4`,
			orgID, employeeID, policyID, period))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: leave balance", ErrNotFound)
		}
		return err
	})
	if err != nil {
		return LeaveBalance{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListLeaveBalances(ctx context.Context, orgID string, f LeaveBalanceFilter, p Page) ([]LeaveBalance, string, error) {
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
	var out []LeaveBalance
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+leaveBalanceCols+` FROM leave_balances
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR employee_id = NULLIF($2,'')::uuid)
			  AND ($3 = '' OR policy_id = NULLIF($3,'')::uuid)
			  AND ($4 = '' OR period = $4)
			  AND (NOT $5 OR (created_at, id) > ($6::timestamptz, NULLIF($7,'')::uuid))
			ORDER BY created_at, id
			LIMIT $8`,
			orgID, f.EmployeeID, f.PolicyID, f.Period, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (LeaveBalance, error) {
			return scanLeaveBalance(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list leave balances: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *PostgresStore) RecalculateLeaveBalance(ctx context.Context, orgID, employeeID, policyID string, asOf time.Time, holidays []Holiday) (LeaveBalance, error) {
	if err := ctx.Err(); err != nil {
		return LeaveBalance{}, err
	}
	var out LeaveBalance
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		pol, err := policyTx(ctx, tx, orgID, policyID)
		if err != nil {
			return err
		}
		emp, err := employeeTx(ctx, tx, orgID, employeeID)
		if err != nil {
			return err
		}
		bal, _, err := computeBalanceTx(ctx, tx, orgID, emp, pol, holidays, asOf, "")
		if err != nil {
			return err
		}
		id := newID()
		out, err = scanLeaveBalance(tx.QueryRow(ctx, `
			INSERT INTO leave_balances
				(id, organization_id, employee_id, policy_id, period, accrued, taken, carried, expired, balance, computed_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (organization_id, employee_id, policy_id, period)
			DO UPDATE SET accrued = EXCLUDED.accrued, taken = EXCLUDED.taken,
				carried = EXCLUDED.carried, expired = EXCLUDED.expired,
				balance = EXCLUDED.balance, computed_at = EXCLUDED.computed_at,
				version = leave_balances.version + 1
			RETURNING `+leaveBalanceCols,
			id, orgID, employeeID, policyID, bal.Period,
			bal.Accrued, bal.Taken, bal.Carried, bal.Expired, bal.Balance, bal.ComputedAt))
		return mapPgError(err, "leave balance")
	})
	if err != nil {
		return LeaveBalance{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Leave requests
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateTimeOffRequest(ctx context.Context, r TimeOffRequest, holidays []Holiday) (TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, err
	}
	if err := r.Validate(); err != nil {
		return TimeOffRequest{}, err
	}
	if r.ID == "" {
		r.ID = newID()
	}
	var out TimeOffRequest
	err := s.inTenant(ctx, r.OrganizationID, func(tx pgx.Tx) error {
		pol, err := policyTx(ctx, tx, r.OrganizationID, r.PolicyID)
		if err != nil {
			return err
		}
		emp, err := employeeTx(ctx, tx, r.OrganizationID, r.EmployeeID)
		if err != nil {
			return err
		}
		var calID any
		if r.HolidayCalendarID != nil {
			var ok bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM holiday_calendars WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*r.HolidayCalendarID, r.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: holiday calendar %s", ErrNotFound, *r.HolidayCalendarID)
			}
			calID = *r.HolidayCalendarID
		}
		start, end := r.startTime(), r.endTime()
		days := WorkingDays(start, end, holidays)
		if days <= 0 {
			return &ValidationError{Field: "start_date", Message: "request covers no working days"}
		}
		asOf := start
		existing, err := requestsTx(ctx, tx, r.OrganizationID, r.EmployeeID, r.PolicyID, "")
		if err != nil {
			return err
		}
		// Sufficiency check against the balance as it stands without this
		// request, including carry-forward from the stored previous period.
		var prev *LeaveBalance
		pb, err := scanLeaveBalance(tx.QueryRow(ctx, `
			SELECT `+leaveBalanceCols+` FROM leave_balances
			WHERE organization_id = $1::uuid AND employee_id = $2::uuid
			  AND policy_id = $3::uuid AND period = $4`,
			r.OrganizationID, r.EmployeeID, r.PolicyID, prevPeriod(asOf.UTC().Format("2006"))))
		if err == nil {
			prev = &pb
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, raw := finalizeBalance(RecalculateBalance(pol, emp, existing, holidays, asOf), prev, pol, asOf)
		if !r.IsUnpaid && days > raw+1e-9 {
			return fmt.Errorf("%w: need %.2f days, have %.2f", ErrInsufficientBalance, days, raw)
		}
		approvers := dedupeApprovers(r.ApproverIDs)
		for _, aid := range approvers {
			if err := validateApproverTx(ctx, tx, r.OrganizationID, aid); err != nil {
				return err
			}
		}
		// Snapshot the balance including this request for auditability.
		candidate := TimeOffRequest{
			EmployeeID: r.EmployeeID, PolicyID: r.PolicyID,
			StartDate: r.StartDate, EndDate: r.EndDate,
			Status: TimeOffRequested, IsUnpaid: r.IsUnpaid,
		}
		withCandidate := append(append([]TimeOffRequest(nil), existing...), candidate)
		snapBal, _ := finalizeBalance(RecalculateBalance(pol, emp, withCandidate, holidays, asOf), prev, pol, asOf)
		snap := snapshotJSON(pol, snapBal)

		out, err = scanTimeOffRequest(tx.QueryRow(ctx, `
			INSERT INTO time_off_requests
				(id, organization_id, employee_id, policy_id, holiday_calendar_id,
				 start_date, end_date, days, snapshot, status, is_unpaid)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5::uuid,
			        $6::date, $7::date, $8, $9, 'requested', $10)
			RETURNING `+timeOffRequestCols,
			r.ID, r.OrganizationID, r.EmployeeID, r.PolicyID, calID,
			r.StartDate, r.EndDate, days, []byte(snap), r.IsUnpaid))
		if err != nil {
			return mapPgError(err, "time-off request")
		}
		for i, aid := range approvers {
			if _, err := tx.Exec(ctx, `
				INSERT INTO time_off_approvals
					(id, organization_id, request_id, sequence, approver_employee_id, status)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, 'pending')`,
				newID(), r.OrganizationID, r.ID, i+1, aid); err != nil {
				return mapPgError(err, "time-off approval")
			}
		}
		return nil
	})
	if err != nil {
		return TimeOffRequest{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetTimeOffRequest(ctx context.Context, orgID, id string) (TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, err
	}
	var out TimeOffRequest
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanTimeOffRequest(tx.QueryRow(ctx, `
			SELECT `+timeOffRequestCols+` FROM time_off_requests
			WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: time-off request %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return TimeOffRequest{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateTimeOffRequest(ctx context.Context, r TimeOffRequest, holidays []Holiday) (TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, err
	}
	if err := r.Validate(); err != nil {
		return TimeOffRequest{}, err
	}
	var out TimeOffRequest
	err := s.inTenant(ctx, r.OrganizationID, func(tx pgx.Tx) error {
		cur, err := scanTimeOffRequest(tx.QueryRow(ctx, `
			SELECT `+timeOffRequestCols+` FROM time_off_requests
			WHERE id = $1::uuid AND organization_id = $2::uuid FOR UPDATE`, r.ID, r.OrganizationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: time-off request %s", ErrNotFound, r.ID)
		}
		if err != nil {
			return err
		}
		if r.Version != cur.Version {
			return fmt.Errorf("%w: time-off request %s", ErrVersionConflict, r.ID)
		}
		if cur.Status != TimeOffRequested {
			return fmt.Errorf("%w: cannot edit a %s request", ErrInvalidTransition, cur.Status)
		}
		pol, err := policyTx(ctx, tx, r.OrganizationID, r.PolicyID)
		if err != nil {
			return err
		}
		emp, err := employeeTx(ctx, tx, r.OrganizationID, r.EmployeeID)
		if err != nil {
			return err
		}
		var calID any
		if r.HolidayCalendarID != nil {
			var ok bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM holiday_calendars WHERE id = $1::uuid AND organization_id = $2::uuid)`,
				*r.HolidayCalendarID, r.OrganizationID).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: holiday calendar %s", ErrNotFound, *r.HolidayCalendarID)
			}
			calID = *r.HolidayCalendarID
		}
		start, end := r.startTime(), r.endTime()
		days := WorkingDays(start, end, holidays)
		if days <= 0 {
			return &ValidationError{Field: "start_date", Message: "request covers no working days"}
		}
		if !r.IsUnpaid {
			_, raw, err := computeBalanceTx(ctx, tx, r.OrganizationID, emp, pol, holidays, start, r.ID)
			if err != nil {
				return err
			}
			if days > raw+1e-9 {
				return fmt.Errorf("%w: need %.2f days, have %.2f", ErrInsufficientBalance, days, raw)
			}
		}
		out, err = scanTimeOffRequest(tx.QueryRow(ctx, `
			UPDATE time_off_requests
			SET employee_id = $2::uuid, policy_id = $3::uuid, holiday_calendar_id = $4::uuid,
			    start_date = $5::date, end_date = $6::date, days = $7, is_unpaid = $8,
			    version = version + 1
			WHERE id = $1::uuid AND organization_id = $9::uuid AND version = $10
			RETURNING `+timeOffRequestCols,
			r.ID, r.EmployeeID, r.PolicyID, calID, r.StartDate, r.EndDate,
			days, r.IsUnpaid, r.OrganizationID, r.Version))
		if errors.Is(err, pgx.ErrNoRows) {
			return versionOutcome(ctx, tx, "time_off_requests", r.OrganizationID, r.ID)
		}
		return mapPgError(err, "time-off request")
	})
	if err != nil {
		return TimeOffRequest{}, err
	}
	return out, nil
}

func (s *PostgresStore) CancelTimeOffRequest(ctx context.Context, orgID, id string, version int64) (TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, err
	}
	var out TimeOffRequest
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		cur, err := scanTimeOffRequest(tx.QueryRow(ctx, `
			SELECT `+timeOffRequestCols+` FROM time_off_requests
			WHERE id = $1::uuid AND organization_id = $2::uuid FOR UPDATE`, id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: time-off request %s", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if version != cur.Version {
			return fmt.Errorf("%w: time-off request %s", ErrVersionConflict, id)
		}
		if cur.Status != TimeOffRequested && cur.Status != TimeOffApproved {
			return fmt.Errorf("%w: cannot cancel a %s request", ErrInvalidTransition, cur.Status)
		}
		var err2 error
		out, err2 = scanTimeOffRequest(tx.QueryRow(ctx, `
			UPDATE time_off_requests
			SET status = 'cancelled', version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid
			RETURNING `+timeOffRequestCols, id, orgID))
		return err2
	})
	if err != nil {
		return TimeOffRequest{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListTimeOffRequests(ctx context.Context, orgID string, f TimeOffRequestFilter, p Page) ([]TimeOffRequest, string, error) {
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
	var out []TimeOffRequest
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+timeOffRequestCols+` FROM time_off_requests
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR employee_id = NULLIF($2,'')::uuid)
			  AND ($3 = '' OR policy_id = NULLIF($3,'')::uuid)
			  AND ($4 = '' OR status = $4)
			  AND (NOT $5 OR (created_at, id) > ($6::timestamptz, NULLIF($7,'')::uuid))
			ORDER BY created_at, id
			LIMIT $8`,
			orgID, f.EmployeeID, f.PolicyID, string(f.Status), haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimeOffRequest, error) {
			return scanTimeOffRequest(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list time-off requests: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *PostgresStore) ListApprovedLeaveOnDate(ctx context.Context, orgID, date string, p Page) ([]TimeOffRequest, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if _, err := parseLeaveDate(date); err != nil {
		return nil, "", &ValidationError{Field: "date", Message: "must be YYYY-MM-DD"}
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
	var out []TimeOffRequest
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+timeOffRequestCols+` FROM time_off_requests
			WHERE organization_id = $1::uuid AND status = 'approved'
			  AND start_date <= $2::date AND end_date >= $2::date
			  AND (NOT $3 OR (created_at, id) > ($4::timestamptz, NULLIF($5,'')::uuid))
			ORDER BY created_at, id
			LIMIT $6`, orgID, date, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimeOffRequest, error) {
			return scanTimeOffRequest(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list approved leave: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Approval chains
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateTimeOffApproval(ctx context.Context, a TimeOffApproval) (TimeOffApproval, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffApproval{}, err
	}
	if err := a.Validate(); err != nil {
		return TimeOffApproval{}, err
	}
	if a.ID == "" {
		a.ID = newID()
	}
	var out TimeOffApproval
	err := s.inTenant(ctx, a.OrganizationID, func(tx pgx.Tx) error {
		var status string
		err := tx.QueryRow(ctx,
			`SELECT status FROM time_off_requests WHERE id = $1::uuid AND organization_id = $2::uuid`,
			a.RequestID, a.OrganizationID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: time-off request %s", ErrNotFound, a.RequestID)
		}
		if err != nil {
			return err
		}
		if TimeOffStatus(status) != TimeOffRequested {
			return fmt.Errorf("%w: cannot extend the chain of a %s request", ErrInvalidTransition, status)
		}
		if err := validateApproverTx(ctx, tx, a.OrganizationID, a.ApproverEmployeeID); err != nil {
			return err
		}
		seq := a.Sequence
		if seq <= 0 {
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(MAX(sequence), 0) + 1 FROM time_off_approvals
				 WHERE request_id = $1::uuid AND organization_id = $2::uuid`,
				a.RequestID, a.OrganizationID).Scan(&seq); err != nil {
				return err
			}
		}
		out, err = scanTimeOffApproval(tx.QueryRow(ctx, `
			INSERT INTO time_off_approvals
				(id, organization_id, request_id, sequence, approver_employee_id, status)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::uuid, 'pending')
			RETURNING `+timeOffApprovalCols,
			a.ID, a.OrganizationID, a.RequestID, seq, a.ApproverEmployeeID))
		return mapPgError(err, "time-off approval")
	})
	if err != nil {
		return TimeOffApproval{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListTimeOffApprovals(ctx context.Context, orgID, requestID string, p Page) ([]TimeOffApproval, string, error) {
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
	var out []TimeOffApproval
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM time_off_requests WHERE id = $1::uuid AND organization_id = $2::uuid)`,
			requestID, orgID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: time-off request %s", ErrNotFound, requestID)
		}
		// Sequence order for display; steps are created in sequence order so
		// the (created_at, id) cursor stays consistent with it.
		rows, err := tx.Query(ctx, `
			SELECT `+timeOffApprovalCols+` FROM time_off_approvals
			WHERE organization_id = $1::uuid AND request_id = $2::uuid
			  AND (NOT $3 OR (created_at, id) > ($4::timestamptz, NULLIF($5,'')::uuid))
			ORDER BY sequence, created_at, id
			LIMIT $6`, orgID, requestID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimeOffApproval, error) {
			return scanTimeOffApproval(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list time-off approvals: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// approvalsTx loads a request's steps ordered by sequence.
func approvalsTx(ctx context.Context, tx pgx.Tx, orgID, requestID string) ([]TimeOffApproval, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+timeOffApprovalCols+` FROM time_off_approvals
		WHERE organization_id = $1::uuid AND request_id = $2::uuid
		ORDER BY sequence`, orgID, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TimeOffApproval, error) {
		return scanTimeOffApproval(r)
	})
}

func (s *PostgresStore) DecideTimeOffApproval(ctx context.Context, orgID, requestID, approverEmployeeID string, approved bool, requestVersion int64, asAdmin bool) (TimeOffRequest, []TimeOffApproval, error) {
	if err := ctx.Err(); err != nil {
		return TimeOffRequest{}, nil, err
	}
	var outReq TimeOffRequest
	var outSteps []TimeOffApproval
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		req, err := scanTimeOffRequest(tx.QueryRow(ctx, `
			SELECT `+timeOffRequestCols+` FROM time_off_requests
			WHERE id = $1::uuid AND organization_id = $2::uuid FOR UPDATE`, requestID, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: time-off request %s", ErrNotFound, requestID)
		}
		if err != nil {
			return err
		}
		if requestVersion != req.Version {
			return fmt.Errorf("%w: time-off request %s", ErrVersionConflict, requestID)
		}
		steps, err := approvalsTx(ctx, tx, orgID, requestID)
		if err != nil {
			return err
		}
		step, newStatus, err := resolveDecision(&req, steps, approverEmployeeID, approved, asAdmin)
		if err != nil {
			return err
		}
		if step != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE time_off_approvals
				SET status = $2, decided_by = $3, decided_at = $4
				WHERE id = $1::uuid`, step.ID, string(step.Status), step.DecidedBy, step.DecidedAt); err != nil {
				return err
			}
		}
		outReq, err = scanTimeOffRequest(tx.QueryRow(ctx, `
			UPDATE time_off_requests
			SET status = $2, version = version + 1
			WHERE id = $1::uuid AND organization_id = $3::uuid
			RETURNING `+timeOffRequestCols, requestID, string(newStatus), orgID))
		if err != nil {
			return err
		}
		outSteps, err = approvalsTx(ctx, tx, orgID, requestID)
		return err
	})
	if err != nil {
		return TimeOffRequest{}, nil, err
	}
	return outReq, outSteps, nil
}

func (s *PostgresStore) BulkApproveTimeOffRequests(ctx context.Context, orgID string, ids []string, approverEmployeeID string, asAdmin bool) ([]TimeOffRequest, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, &ValidationError{Field: "ids", Message: "at least one request id is required"}
	}
	var out []TimeOffRequest
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		type candidate struct {
			req   TimeOffRequest
			steps []TimeOffApproval
		}
		cands := make([]candidate, 0, len(ids))
		results := make([]BulkApprovalResult, 0, len(ids))
		failed := false
		for _, id := range ids {
			res := BulkApprovalResult{RequestID: id}
			req, err := scanTimeOffRequest(tx.QueryRow(ctx, `
				SELECT `+timeOffRequestCols+` FROM time_off_requests
				WHERE id = $1::uuid AND organization_id = $2::uuid FOR UPDATE`, id, orgID))
			if errors.Is(err, pgx.ErrNoRows) {
				res.Reason = "not found"
			} else if err != nil {
				return err
			} else if req.Status != TimeOffRequested {
				res.Reason = "request is " + string(req.Status)
			} else {
				steps, err := approvalsTx(ctx, tx, orgID, id)
				if err != nil {
					return err
				}
				var cur *TimeOffApproval
				for i := range steps {
					if steps[i].Status == ApprovalPending {
						cur = &steps[i]
						break
					}
				}
				switch {
				case cur == nil && !asAdmin:
					res.Reason = "no pending approval step"
				case cur != nil && cur.ApproverEmployeeID != approverEmployeeID && !asAdmin:
					res.Reason = "not the current approver"
				default:
					res.Approved = true
					cands = append(cands, candidate{req: req, steps: steps})
				}
			}
			if !res.Approved {
				failed = true
			}
			results = append(results, res)
		}
		if failed {
			// Nothing applied yet in this transaction; returning an error
			// rolls it back, keeping the operation all-or-nothing.
			return &BulkApprovalError{Results: results}
		}
		out = make([]TimeOffRequest, 0, len(cands))
		for _, c := range cands {
			req := c.req
			step, newStatus, err := resolveDecision(&req, c.steps, approverEmployeeID, true, asAdmin)
			if err != nil {
				// Unreachable: validated above inside the same transaction.
				return err
			}
			if step != nil {
				if _, err := tx.Exec(ctx, `
					UPDATE time_off_approvals
					SET status = $2, decided_by = $3, decided_at = $4
					WHERE id = $1::uuid`, step.ID, string(step.Status), step.DecidedBy, step.DecidedAt); err != nil {
					return err
				}
			}
			updated, err := scanTimeOffRequest(tx.QueryRow(ctx, `
				UPDATE time_off_requests
				SET status = $2, version = version + 1
				WHERE id = $1::uuid AND organization_id = $3::uuid
				RETURNING `+timeOffRequestCols, req.ID, string(newStatus), orgID))
			if err != nil {
				return err
			}
			out = append(out, updated)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Holiday calendars and holidays
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreateHolidayCalendar(ctx context.Context, c HolidayCalendar) (HolidayCalendar, error) {
	if err := ctx.Err(); err != nil {
		return HolidayCalendar{}, err
	}
	if err := c.Validate(); err != nil {
		return HolidayCalendar{}, err
	}
	if c.ID == "" {
		c.ID = newID()
	}
	var out HolidayCalendar
	err := s.inTenant(ctx, c.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanHolidayCalendar(tx.QueryRow(ctx, `
			INSERT INTO holiday_calendars (id, organization_id, name, country_code)
			VALUES ($1::uuid, $2::uuid, $3, $4)
			RETURNING `+holidayCalendarCols,
			c.ID, c.OrganizationID, c.Name, c.CountryCode))
		return mapPgError(err, "holiday calendar")
	})
	if err != nil {
		return HolidayCalendar{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetHolidayCalendar(ctx context.Context, orgID, id string) (HolidayCalendar, error) {
	if err := ctx.Err(); err != nil {
		return HolidayCalendar{}, err
	}
	var out HolidayCalendar
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanHolidayCalendar(tx.QueryRow(ctx, `
			SELECT `+holidayCalendarCols+` FROM holiday_calendars
			WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: holiday calendar %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return HolidayCalendar{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdateHolidayCalendar(ctx context.Context, c HolidayCalendar) (HolidayCalendar, error) {
	if err := ctx.Err(); err != nil {
		return HolidayCalendar{}, err
	}
	if err := c.Validate(); err != nil {
		return HolidayCalendar{}, err
	}
	var out HolidayCalendar
	err := s.inTenant(ctx, c.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanHolidayCalendar(tx.QueryRow(ctx, `
			UPDATE holiday_calendars
			SET name = $2, country_code = $3, version = version + 1
			WHERE id = $1::uuid AND organization_id = $4::uuid AND version = $5
			RETURNING `+holidayCalendarCols,
			c.ID, c.Name, c.CountryCode, c.OrganizationID, c.Version))
		if errors.Is(err, pgx.ErrNoRows) {
			return versionOutcome(ctx, tx, "holiday_calendars", c.OrganizationID, c.ID)
		}
		return mapPgError(err, "holiday calendar")
	})
	if err != nil {
		return HolidayCalendar{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteHolidayCalendar(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM holiday_calendars WHERE id = $1::uuid AND organization_id = $2::uuid)`,
			id, orgID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: holiday calendar %s", ErrNotFound, id)
		}
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM holidays WHERE calendar_id = $1::uuid AND organization_id = $2::uuid)`,
			id, orgID).Scan(&ok); err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("%w: calendar %s still has holidays", ErrNotEmpty, id)
		}
		_, err := tx.Exec(ctx,
			`DELETE FROM holiday_calendars WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		return err
	})
}

func (s *PostgresStore) ListHolidayCalendars(ctx context.Context, orgID string, p Page) ([]HolidayCalendar, string, error) {
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
	var out []HolidayCalendar
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+holidayCalendarCols+` FROM holiday_calendars
			WHERE organization_id = $1::uuid
			  AND (NOT $2 OR (created_at, id) > ($3::timestamptz, NULLIF($4,'')::uuid))
			ORDER BY created_at, id
			LIMIT $5`, orgID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (HolidayCalendar, error) {
			return scanHolidayCalendar(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list holiday calendars: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *PostgresStore) CreateHoliday(ctx context.Context, h Holiday) (Holiday, error) {
	if err := ctx.Err(); err != nil {
		return Holiday{}, err
	}
	if err := h.Validate(); err != nil {
		return Holiday{}, err
	}
	if h.ID == "" {
		h.ID = newID()
	}
	var out Holiday
	err := s.inTenant(ctx, h.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanHoliday(tx.QueryRow(ctx, `
			INSERT INTO holidays (id, organization_id, calendar_id, date, name)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::date, $5)
			RETURNING `+holidayCols,
			h.ID, h.OrganizationID, h.CalendarID, h.Date, h.Name))
		return mapPgError(err, "holiday")
	})
	if err != nil {
		return Holiday{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetHoliday(ctx context.Context, orgID, id string) (Holiday, error) {
	if err := ctx.Err(); err != nil {
		return Holiday{}, err
	}
	var out Holiday
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanHoliday(tx.QueryRow(ctx, `
			SELECT `+holidayCols+` FROM holidays
			WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: holiday %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return Holiday{}, err
	}
	return out, nil
}

func (s *PostgresStore) DeleteHoliday(ctx context.Context, orgID, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		res, err := tx.Exec(ctx,
			`DELETE FROM holidays WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID)
		if err != nil {
			return err
		}
		if res.RowsAffected() == 0 {
			return fmt.Errorf("%w: holiday %s", ErrNotFound, id)
		}
		return nil
	})
}

func (s *PostgresStore) ListHolidays(ctx context.Context, orgID, calendarID string, p Page) ([]Holiday, string, error) {
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
	var out []Holiday
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM holiday_calendars WHERE id = $1::uuid AND organization_id = $2::uuid)`,
			calendarID, orgID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: holiday calendar %s", ErrNotFound, calendarID)
		}
		// Date order for display; the cursor keys on (date, id).
		rows, err := tx.Query(ctx, `
			SELECT `+holidayCols+` FROM holidays
			WHERE organization_id = $1::uuid AND calendar_id = $2::uuid
			  AND (NOT $3 OR (date, id) > ($4::date, NULLIF($5,'')::uuid))
			ORDER BY date, id
			LIMIT $6`, orgID, calendarID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Holiday, error) {
			return scanHoliday(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list holidays: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.dateTime(), last.ID)
	}
	return out, next, nil
}
