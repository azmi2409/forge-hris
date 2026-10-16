package store

// PostgresStore payroll implementation (component 5). Every org-scoped
// operation runs inside s.inTenant (app.tenant_id + explicit
// organization_id predicates); version-checked writes map 0-row outcomes via
// versionOutcome; constraint violations via mapPgError. System presets come
// from payroll.SeedPresets and never touch the database.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/azmi2409/forge-hris/payroll"
)

// Compile-time contract check.
var _ PayrollStore = (*PostgresStore)(nil)

// ---------------------------------------------------------------------------
// Row scanners. UUID columns are selected as ::text so they scan straight
// into Go strings; nullable columns scan into pointers.
// ---------------------------------------------------------------------------

func scanPayrollRun(r rowScanner) (PayrollRun, error) {
	var pr PayrollRun
	var status string
	var rpv *string
	err := r.Scan(&pr.ID, &pr.OrganizationID, &pr.Period, &pr.CountryCode,
		&status, &rpv, &pr.Version, &pr.CreatedAt, &pr.UpdatedAt)
	if err != nil {
		return PayrollRun{}, err
	}
	pr.Status = PayrollRunStatus(status)
	if rpv != nil {
		pr.RulePackVersion = *rpv
	}
	return pr, nil
}

const payrollRunCols = `id::text, organization_id::text, period, country_code, status,
	rule_pack_version, version, created_at, updated_at`

func scanEmployeePayLine(r rowScanner) (EmployeePayLine, error) {
	var l EmployeePayLine
	var itemsJSON, statJSON []byte
	err := r.Scan(&l.ID, &l.OrganizationID, &l.RunID, &l.EmployeeID,
		&l.GrossMinor, &l.AdditionsMinor, &l.DeductionsMinor, &l.NetMinor,
		&itemsJSON, &statJSON, &l.BankAccount, &l.BankCode,
		&l.Version, &l.CreatedAt, &l.UpdatedAt)
	if err != nil {
		return EmployeePayLine{}, err
	}
	if len(itemsJSON) > 0 {
		if err := json.Unmarshal(itemsJSON, &l.Items); err != nil {
			return EmployeePayLine{}, fmt.Errorf("store: decode pay line items: %w", err)
		}
	}
	if len(statJSON) > 0 {
		if err := json.Unmarshal(statJSON, &l.Statutory); err != nil {
			return EmployeePayLine{}, fmt.Errorf("store: decode pay line statutory: %w", err)
		}
	}
	return l, nil
}

const payLineCols = `id::text, organization_id::text, run_id::text, employee_id::text,
	gross_minor, additions_minor, deductions_minor, net_minor, items, statutory,
	bank_account, bank_code, version, created_at, updated_at`

func scanPayrollItemPreset(r rowScanner) (PayrollItemPreset, error) {
	var pr PayrollItemPreset
	var kind string
	err := r.Scan(&pr.ID, &pr.OrganizationID, &pr.Code, &pr.Name, &kind,
		&pr.SubjectToEPF, &pr.SubjectToSOCSO, &pr.SubjectToEIS, &pr.SubjectToPCB,
		&pr.SubjectToBPJSKes, &pr.SubjectToJHT, &pr.SubjectToJKK, &pr.SubjectToJKM,
		&pr.SubjectToJP, &pr.SubjectToPPh21, &pr.ReducesContributionWages,
		&pr.Version, &pr.CreatedAt, &pr.UpdatedAt)
	if err != nil {
		return PayrollItemPreset{}, err
	}
	pr.Kind = payroll.PresetKind(kind)
	return pr, nil
}

const payPresetCols = `id::text, organization_id::text, code, name, kind,
	subject_to_epf, subject_to_socso, subject_to_eis, subject_to_pcb,
	subject_to_bpjs_kes, subject_to_jht, subject_to_jkk, subject_to_jkm,
	subject_to_jp, subject_to_pph21, reduces_contribution_wages,
	version, created_at, updated_at`

func scanSyncItem(r rowScanner) (SyncItem, error) {
	var it SyncItem
	var kind string
	err := r.Scan(&it.ID, &it.OrganizationID, &it.RunID, &it.EmployeeID,
		&it.Source, &it.ExternalID, &it.Label, &kind, &it.PresetCode,
		&it.AmountMinor, &it.CreatedAt)
	if err != nil {
		return SyncItem{}, err
	}
	it.Kind = payroll.PresetKind(kind)
	return it, nil
}

const syncItemCols = `id::text, organization_id::text, run_id::text, employee_id::text,
	source, external_id, label, kind, preset_code, amount_minor, created_at`

func scanPayrollPreview(r rowScanner) (PayrollPreview, error) {
	var p PayrollPreview
	var payload []byte
	var exp *time.Time
	err := r.Scan(&p.ID, &p.OrganizationID, &p.RunID, &payload, &exp, &p.CreatedAt)
	if err != nil {
		return PayrollPreview{}, err
	}
	p.Payload = json.RawMessage(payload)
	if exp != nil {
		p.ExpiresAt = *exp
	}
	return p, nil
}

const payrollPreviewCols = `id::text, organization_id::text, run_id::text,
	payload, expires_at, created_at`

// runStatusTx reads a run's status inside a tenant transaction.
func runStatusTx(ctx context.Context, tx pgx.Tx, orgID, runID string) (PayrollRunStatus, error) {
	var status string
	err := tx.QueryRow(ctx,
		`SELECT status FROM payroll_runs WHERE id = $1::uuid AND organization_id = $2::uuid`,
		runID, orgID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: payroll run %s", ErrNotFound, runID)
	}
	if err != nil {
		return "", err
	}
	return PayrollRunStatus(status), nil
}

// ---------------------------------------------------------------------------
// Item presets
// ---------------------------------------------------------------------------

// systemPresets builds the seed catalog as store presets (never stored).
func systemPresets() []PayrollItemPreset {
	seeds := payroll.SeedPresets()
	out := make([]PayrollItemPreset, 0, len(seeds))
	for _, sp := range seeds {
		pr := PayrollToPreset(sp)
		pr.ID = systemPresetPrefix + sp.Code
		pr.IsSystem = true
		out = append(out, pr)
	}
	return out
}

func (s *PostgresStore) ListPayrollPresets(ctx context.Context, orgID string, p Page) ([]PayrollItemPreset, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	sys := systemPresets()
	key := func(pr PayrollItemPreset) cursorKey { return cursorKey{pr.CreatedAt, pr.ID} }
	sortByCursor(sys, key)
	var org []PayrollItemPreset
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+payPresetCols+` FROM payroll_item_presets
			WHERE organization_id = $1::uuid
			ORDER BY created_at, id`, orgID)
		if err != nil {
			return err
		}
		defer rows.Close()
		org, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayrollItemPreset, error) {
			return scanPayrollItemPreset(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list payroll presets: %w", err)
	}
	sortByCursor(org, key)
	return paginate(append(sys, org...), key, p)
}

func (s *PostgresStore) GetPayrollPreset(ctx context.Context, orgID, id string) (PayrollItemPreset, error) {
	if err := ctx.Err(); err != nil {
		return PayrollItemPreset{}, err
	}
	if pr, ok := resolveSystemPreset(id); ok {
		return pr, nil
	}
	var out PayrollItemPreset
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayrollItemPreset(tx.QueryRow(ctx, `
			SELECT `+payPresetCols+` FROM payroll_item_presets
			WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: payroll item preset %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return PayrollItemPreset{}, err
	}
	return out, nil
}

func (s *PostgresStore) CreatePayrollPreset(ctx context.Context, orgID string, pr PayrollItemPreset) (PayrollItemPreset, error) {
	if err := ctx.Err(); err != nil {
		return PayrollItemPreset{}, err
	}
	pr.OrganizationID = orgID
	if err := pr.Validate(); err != nil {
		return PayrollItemPreset{}, err
	}
	if _, clash := seedPresetByCode()[pr.Code]; clash {
		return PayrollItemPreset{}, fmt.Errorf("%w: preset code %q collides with a system preset", ErrAlreadyExists, pr.Code)
	}
	if pr.ID == "" {
		pr.ID = newID()
	}
	var out PayrollItemPreset
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayrollItemPreset(tx.QueryRow(ctx, `
			INSERT INTO payroll_item_presets (id, organization_id, code, name, kind,
				subject_to_epf, subject_to_socso, subject_to_eis, subject_to_pcb,
				subject_to_bpjs_kes, subject_to_jht, subject_to_jkk, subject_to_jkm,
				subject_to_jp, subject_to_pph21, reduces_contribution_wages)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			RETURNING `+payPresetCols,
			pr.ID, orgID, pr.Code, pr.Name, string(pr.Kind),
			pr.SubjectToEPF, pr.SubjectToSOCSO, pr.SubjectToEIS, pr.SubjectToPCB,
			pr.SubjectToBPJSKes, pr.SubjectToJHT, pr.SubjectToJKK, pr.SubjectToJKM,
			pr.SubjectToJP, pr.SubjectToPPh21, pr.ReducesContributionWages))
		return mapPgError(err, "payroll item preset")
	})
	if err != nil {
		return PayrollItemPreset{}, err
	}
	return out, nil
}

func (s *PostgresStore) UpdatePayrollPreset(ctx context.Context, orgID string, pr PayrollItemPreset) (PayrollItemPreset, error) {
	if err := ctx.Err(); err != nil {
		return PayrollItemPreset{}, err
	}
	if _, ok := resolveSystemPreset(pr.ID); ok {
		return PayrollItemPreset{}, fmt.Errorf("%w: preset %s", ErrSystemPresetImmutable, pr.ID)
	}
	pr.OrganizationID = orgID
	if err := pr.Validate(); err != nil {
		return PayrollItemPreset{}, err
	}
	if _, clash := seedPresetByCode()[pr.Code]; clash {
		return PayrollItemPreset{}, fmt.Errorf("%w: preset code %q collides with a system preset", ErrAlreadyExists, pr.Code)
	}
	var out PayrollItemPreset
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayrollItemPreset(tx.QueryRow(ctx, `
			UPDATE payroll_item_presets
			SET code = $2, name = $3, kind = $4,
			    subject_to_epf = $5, subject_to_socso = $6, subject_to_eis = $7,
			    subject_to_pcb = $8, subject_to_bpjs_kes = $9, subject_to_jht = $10,
			    subject_to_jkk = $11, subject_to_jkm = $12, subject_to_jp = $13,
			    subject_to_pph21 = $14, reduces_contribution_wages = $15,
			    version = version + 1
			WHERE id = $1::uuid AND organization_id = $16::uuid AND version = $17
			RETURNING `+payPresetCols,
			pr.ID, pr.Code, pr.Name, string(pr.Kind),
			pr.SubjectToEPF, pr.SubjectToSOCSO, pr.SubjectToEIS, pr.SubjectToPCB,
			pr.SubjectToBPJSKes, pr.SubjectToJHT, pr.SubjectToJKK, pr.SubjectToJKM,
			pr.SubjectToJP, pr.SubjectToPPh21, pr.ReducesContributionWages,
			orgID, pr.Version))
		if errors.Is(err, pgx.ErrNoRows) {
			return versionOutcome(ctx, tx, "payroll_item_presets", orgID, pr.ID)
		}
		return mapPgError(err, "payroll item preset")
	})
	if err != nil {
		return PayrollItemPreset{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Payroll runs
// ---------------------------------------------------------------------------

func (s *PostgresStore) CreatePayrollRun(ctx context.Context, r PayrollRun) (PayrollRun, error) {
	if err := ctx.Err(); err != nil {
		return PayrollRun{}, err
	}
	r.Status = PayrollRunDraft // creation always starts in draft
	if err := r.Validate(); err != nil {
		return PayrollRun{}, err
	}
	if r.ID == "" {
		r.ID = newID()
	}
	var out PayrollRun
	err := s.inTenant(ctx, r.OrganizationID, func(tx pgx.Tx) error {
		var err error
		var rpv *string
		if r.RulePackVersion != "" {
			rpv = &r.RulePackVersion
		}
		out, err = scanPayrollRun(tx.QueryRow(ctx, `
			INSERT INTO payroll_runs (id, organization_id, period, country_code, status, rule_pack_version)
			VALUES ($1::uuid, $2::uuid, $3, $4, 'draft', $5)
			RETURNING `+payrollRunCols,
			r.ID, r.OrganizationID, r.Period, r.CountryCode, rpv))
		return mapPgError(err, "payroll run")
	})
	if err != nil {
		return PayrollRun{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetPayrollRun(ctx context.Context, orgID, id string) (PayrollRun, error) {
	if err := ctx.Err(); err != nil {
		return PayrollRun{}, err
	}
	var out PayrollRun
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayrollRun(tx.QueryRow(ctx, `
			SELECT `+payrollRunCols+` FROM payroll_runs
			WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: payroll run %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return PayrollRun{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListPayrollRuns(ctx context.Context, orgID string, f PayrollRunFilter, p Page) ([]PayrollRun, string, error) {
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
	var out []PayrollRun
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+payrollRunCols+` FROM payroll_runs
			WHERE organization_id = $1::uuid
			  AND ($2 = '' OR status = $2)
			  AND ($3 = '' OR period = $3)
			  AND (NOT $4 OR (created_at, id) > ($5::timestamptz, NULLIF($6,'')::uuid))
			ORDER BY created_at, id
			LIMIT $7`, orgID, string(f.Status), f.Period, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PayrollRun, error) {
			return scanPayrollRun(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list payroll runs: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *PostgresStore) TransitionPayrollRun(ctx context.Context, orgID, id string, version int64, to PayrollRunStatus, actor string) (PayrollRun, error) {
	if err := ctx.Err(); err != nil {
		return PayrollRun{}, err
	}
	if !ValidPayrollRunStatus(to) {
		return PayrollRun{}, &ValidationError{Field: "status", Message: "unknown payroll run status"}
	}
	_ = actor // informational; callers record it in the audit log
	var out PayrollRun
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var status string
		var cur int64
		err := tx.QueryRow(ctx,
			`SELECT status, version FROM payroll_runs WHERE id = $1::uuid AND organization_id = $2::uuid`,
			id, orgID).Scan(&status, &cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: payroll run %s", ErrNotFound, id)
		}
		if err != nil {
			return err
		}
		if version != cur {
			return fmt.Errorf("%w: payroll run %s", ErrVersionConflict, id)
		}
		if !allowedRunTransition(PayrollRunStatus(status), to) {
			return fmt.Errorf("%w: %s -> %s", ErrIllegalRunTransition, status, to)
		}
		var err2 error
		out, err2 = scanPayrollRun(tx.QueryRow(ctx, `
			UPDATE payroll_runs
			SET status = $3, version = version + 1
			WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $4
			RETURNING `+payrollRunCols, id, orgID, string(to), version))
		return err2
	})
	if err != nil {
		return PayrollRun{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pay lines
// ---------------------------------------------------------------------------

func (s *PostgresStore) UpsertPayLine(ctx context.Context, l EmployeePayLine) (EmployeePayLine, error) {
	if err := ctx.Err(); err != nil {
		return EmployeePayLine{}, err
	}
	if err := l.Validate(); err != nil {
		return EmployeePayLine{}, err
	}
	itemsJSON, err := json.Marshal(l.Items)
	if err != nil {
		return EmployeePayLine{}, fmt.Errorf("store: encode pay line items: %w", err)
	}
	statJSON, err := json.Marshal(l.Statutory)
	if err != nil {
		return EmployeePayLine{}, fmt.Errorf("store: encode pay line statutory: %w", err)
	}
	var out EmployeePayLine
	err = s.inTenant(ctx, l.OrganizationID, func(tx pgx.Tx) error {
		status, err := runStatusTx(ctx, tx, l.OrganizationID, l.RunID)
		if err != nil {
			return err
		}
		if status != PayrollRunDraft {
			return fmt.Errorf("%w: payroll run %s is %s", ErrRunNotDraft, l.RunID, status)
		}
		if _, err := employeeTx(ctx, tx, l.OrganizationID, l.EmployeeID); err != nil {
			return err
		}
		if l.ID != "" {
			// Version-checked update. Skipped when no ID was given: binding
			// '' to a uuid parameter is a syntax error, and a new line has
			// nothing to update anyway.
			var err2 error
			out, err2 = scanEmployeePayLine(tx.QueryRow(ctx, `
				UPDATE employee_pay_lines
				SET gross_minor = $4, additions_minor = $5, deductions_minor = $6,
				    net_minor = $7, items = $8::jsonb, statutory = $9::jsonb,
				    bank_account = $10, bank_code = $11, version = version + 1
				WHERE id = $1::uuid AND organization_id = $2::uuid AND version = $3
				RETURNING `+payLineCols,
				l.ID, l.OrganizationID, l.Version,
				l.GrossMinor, l.AdditionsMinor, l.DeductionsMinor, l.NetMinor,
				string(itemsJSON), string(statJSON), l.BankAccount, l.BankCode))
			if err2 == nil {
				return nil
			}
			if !errors.Is(err2, pgx.ErrNoRows) {
				return err2
			}
		}
		// Insert when there is no line yet for (run, employee);
		// otherwise the id/version missed -> version conflict.
		var exists bool
		if qerr := tx.QueryRow(ctx, `
				SELECT EXISTS(SELECT 1 FROM employee_pay_lines
					WHERE organization_id = $1::uuid AND run_id = $2::uuid AND employee_id = $3::uuid)`,
			l.OrganizationID, l.RunID, l.EmployeeID).Scan(&exists); qerr != nil {
			return qerr
		}
		if exists {
			return fmt.Errorf("%w: pay line for employee %s", ErrVersionConflict, l.EmployeeID)
		}
		id := l.ID
		if id == "" {
			id = newID()
		}
		var err2 error
		out, err2 = scanEmployeePayLine(tx.QueryRow(ctx, `
				INSERT INTO employee_pay_lines (id, organization_id, run_id, employee_id,
					gross_minor, additions_minor, deductions_minor, net_minor,
					items, statutory, bank_account, bank_code)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8,
					$9::jsonb, $10::jsonb, $11, $12)
				RETURNING `+payLineCols,
			id, l.OrganizationID, l.RunID, l.EmployeeID,
			l.GrossMinor, l.AdditionsMinor, l.DeductionsMinor, l.NetMinor,
			string(itemsJSON), string(statJSON), l.BankAccount, l.BankCode))
		return mapPgError(err2, "pay line")
	})
	if err != nil {
		return EmployeePayLine{}, err
	}
	return out, nil
}

func (s *PostgresStore) ListPayLines(ctx context.Context, orgID, runID string, p Page) ([]EmployeePayLine, string, error) {
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
	var out []EmployeePayLine
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+payLineCols+` FROM employee_pay_lines
			WHERE organization_id = $1::uuid AND run_id = $2::uuid
			  AND (NOT $3 OR (created_at, id) > ($4::timestamptz, NULLIF($5,'')::uuid))
			ORDER BY created_at, id
			LIMIT $6`, orgID, runID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (EmployeePayLine, error) {
			return scanEmployeePayLine(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list pay lines: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

func (s *PostgresStore) GetPayLine(ctx context.Context, orgID, runID, employeeID string) (EmployeePayLine, error) {
	if err := ctx.Err(); err != nil {
		return EmployeePayLine{}, err
	}
	var out EmployeePayLine
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanEmployeePayLine(tx.QueryRow(ctx, `
			SELECT `+payLineCols+` FROM employee_pay_lines
			WHERE organization_id = $1::uuid AND run_id = $2::uuid AND employee_id = $3::uuid`,
			orgID, runID, employeeID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: pay line for employee %s", ErrNotFound, employeeID)
		}
		return err
	})
	if err != nil {
		return EmployeePayLine{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Sync items
// ---------------------------------------------------------------------------

func (s *PostgresStore) AddSyncItems(ctx context.Context, orgID, runID string, items []SyncItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}
	// Validate everything before inserting anything: the add is atomic.
	for i := range items {
		items[i].OrganizationID = orgID
		items[i].RunID = runID
		if err := items[i].Validate(); err != nil {
			return err
		}
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		status, err := runStatusTx(ctx, tx, orgID, runID)
		if err != nil {
			return err
		}
		if status != PayrollRunDraft {
			return fmt.Errorf("%w: payroll run %s is %s", ErrRunNotDraft, runID, status)
		}
		for i := range items {
			if _, err := employeeTx(ctx, tx, orgID, items[i].EmployeeID); err != nil {
				return err
			}
		}
		for i := range items {
			it := items[i]
			id := it.ID
			if id == "" {
				id = newID()
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO payroll_sync_items (id, organization_id, run_id, employee_id,
					source, external_id, label, kind, preset_code, amount_minor)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, $5, $6, $7, $8, $9, $10)`,
				id, orgID, runID, it.EmployeeID,
				it.Source, it.ExternalID, it.Label, string(it.Kind), it.PresetCode, it.AmountMinor); err != nil {
				return mapPgError(err, "sync item")
			}
		}
		return nil
	})
}

func (s *PostgresStore) ListSyncItems(ctx context.Context, orgID, runID string, p Page) ([]SyncItem, string, error) {
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
	var out []SyncItem
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT `+syncItemCols+` FROM payroll_sync_items
			WHERE organization_id = $1::uuid AND run_id = $2::uuid
			  AND (NOT $3 OR (created_at, id) > ($4::timestamptz, NULLIF($5,'')::uuid))
			ORDER BY created_at, id
			LIMIT $6`, orgID, runID, haveCursor, ct, cid, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		out, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SyncItem, error) {
			return scanSyncItem(r)
		})
		return err
	})
	if err != nil {
		return nil, "", fmt.Errorf("store: list sync items: %w", err)
	}
	next := ""
	if len(out) == limit {
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// ---------------------------------------------------------------------------
// Previews
// ---------------------------------------------------------------------------

func (s *PostgresStore) SavePayrollPreview(ctx context.Context, p PayrollPreview) (PayrollPreview, error) {
	if err := ctx.Err(); err != nil {
		return PayrollPreview{}, err
	}
	if err := p.Validate(); err != nil {
		return PayrollPreview{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	var exp *time.Time
	if !p.ExpiresAt.IsZero() {
		exp = &p.ExpiresAt
	}
	var out PayrollPreview
	err := s.inTenant(ctx, p.OrganizationID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayrollPreview(tx.QueryRow(ctx, `
			INSERT INTO payroll_previews (id, organization_id, run_id, payload, expires_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::jsonb, $5)
			RETURNING `+payrollPreviewCols,
			p.ID, p.OrganizationID, p.RunID, string(p.Payload), exp))
		return mapPgError(err, "payroll preview")
	})
	if err != nil {
		return PayrollPreview{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetPayrollPreview(ctx context.Context, orgID, id string) (PayrollPreview, error) {
	if err := ctx.Err(); err != nil {
		return PayrollPreview{}, err
	}
	var out PayrollPreview
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanPayrollPreview(tx.QueryRow(ctx, `
			SELECT `+payrollPreviewCols+` FROM payroll_previews
			WHERE id = $1::uuid AND organization_id = $2::uuid`, id, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: payroll preview %s", ErrNotFound, id)
		}
		return err
	})
	if err != nil {
		return PayrollPreview{}, err
	}
	if !out.ExpiresAt.IsZero() && time.Now().After(out.ExpiresAt) {
		return PayrollPreview{}, fmt.Errorf("%w: payroll preview %s expired", ErrNotFound, id)
	}
	return out, nil
}
