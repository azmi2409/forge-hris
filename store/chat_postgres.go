package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Compile-time contract check for the component 6 storage.
var _ ChatStore = (*PostgresStore)(nil)

const slackWorkspaceCols = `team_id, organization_id::text, team_name,
	enc_signing_secret, enc_service_token, version, created_at, updated_at`

func scanSlackWorkspace(r rowScanner) (SlackWorkspace, error) {
	var w SlackWorkspace
	err := r.Scan(
		&w.TeamID, &w.OrganizationID, &w.TeamName,
		&w.EncSigningSecret, &w.EncServiceToken, &w.Version,
		&w.CreatedAt, &w.UpdatedAt,
	)
	return w, err
}

const teamsConfigCols = `organization_id::text, tenant_ref, enc_shared_secret,
	version, created_at, updated_at`

func scanTeamsConfig(r rowScanner) (TeamsConfig, error) {
	var c TeamsConfig
	err := r.Scan(
		&c.OrganizationID, &c.TenantRef, &c.EncSharedSecret,
		&c.Version, &c.CreatedAt, &c.UpdatedAt,
	)
	return c, err
}

const essTokenCols = `token_hash, organization_id::text, employee_id::text,
	expires_at, used_at, created_at`

func scanESSToken(r rowScanner) (ESSToken, error) {
	var t ESSToken
	err := r.Scan(
		&t.TokenHash, &t.OrganizationID, &t.EmployeeID,
		&t.ExpiresAt, &t.UsedAt, &t.CreatedAt,
	)
	return t, err
}

// ---------------------------------------------------------------------------
// Slack workspaces
// ---------------------------------------------------------------------------

func (s *PostgresStore) SaveSlackWorkspace(ctx context.Context, w SlackWorkspace) (SlackWorkspace, error) {
	if err := ctx.Err(); err != nil {
		return SlackWorkspace{}, err
	}
	if err := w.Validate(); err != nil {
		return SlackWorkspace{}, err
	}
	var out SlackWorkspace
	err := s.inTenant(ctx, w.OrganizationID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`,
			w.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: organization %s", ErrNotFound, w.OrganizationID)
		}
		// Version-checked update when the row exists; insert otherwise.
		err := tx.QueryRow(ctx, `
			UPDATE slack_workspaces
			SET team_name = $2, enc_signing_secret = $3, enc_service_token = $4,
			    version = version + 1
			WHERE team_id = $1 AND version = $5
			RETURNING `+slackWorkspaceCols,
			w.TeamID, w.TeamName, w.EncSigningSecret, w.EncServiceToken, w.Version,
		).Scan(
			&out.TeamID, &out.OrganizationID, &out.TeamName,
			&out.EncSigningSecret, &out.EncServiceToken, &out.Version,
			&out.CreatedAt, &out.UpdatedAt,
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// No row updated: either the workspace is new or the version is
		// stale. Distinguish so callers get ErrVersionConflict, not a
		// duplicate-key surprise.
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM slack_workspaces WHERE team_id = $1)`,
			w.TeamID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: slack workspace %s", ErrVersionConflict, w.TeamID)
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO slack_workspaces
				(team_id, organization_id, team_name, enc_signing_secret, enc_service_token)
			VALUES ($1, $2::uuid, $3, $4, $5)
			RETURNING `+slackWorkspaceCols,
			w.TeamID, w.OrganizationID, w.TeamName, w.EncSigningSecret, w.EncServiceToken,
		).Scan(
			&out.TeamID, &out.OrganizationID, &out.TeamName,
			&out.EncSigningSecret, &out.EncServiceToken, &out.Version,
			&out.CreatedAt, &out.UpdatedAt,
		)
		return mapPgError(err, "slack workspace")
	})
	if err != nil {
		return SlackWorkspace{}, err
	}
	return out, nil
}

// GetSlackWorkspace looks the workspace up by team_id. It is cross-tenant
// by nature (team_id arrives from Slack before the org is known), so it
// reads outside a tenant transaction — the same pattern as
// GetCredentialByTokenHash. The signing secret remains the authorization.
func (s *PostgresStore) GetSlackWorkspace(ctx context.Context, teamID string) (SlackWorkspace, error) {
	if err := ctx.Err(); err != nil {
		return SlackWorkspace{}, err
	}
	out, err := scanSlackWorkspace(s.pool.QueryRow(ctx,
		`SELECT `+slackWorkspaceCols+` FROM slack_workspaces WHERE team_id = $1`, teamID))
	if errors.Is(err, pgx.ErrNoRows) {
		return SlackWorkspace{}, fmt.Errorf("%w: slack workspace %s", ErrNotFound, teamID)
	}
	return out, err
}

func (s *PostgresStore) DeleteSlackWorkspace(ctx context.Context, teamID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The org is needed for tenant scoping; resolve it first (cross-tenant
	// read), then delete inside the tenant.
	ws, err := s.GetSlackWorkspace(ctx, teamID)
	if err != nil {
		return err
	}
	return s.inTenant(ctx, ws.OrganizationID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `DELETE FROM slack_workspaces WHERE team_id = $1`, teamID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("%w: slack workspace %s", ErrNotFound, teamID)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Slack user identity map
// ---------------------------------------------------------------------------

func (s *PostgresStore) MapSlackUser(ctx context.Context, teamID, slackUserID, employeeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if slackUserID == "" {
		return &ValidationError{Field: "slack_user_id", Message: "required"}
	}
	ws, err := s.GetSlackWorkspace(ctx, teamID)
	if err != nil {
		return err
	}
	return s.inTenant(ctx, ws.OrganizationID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid)`,
			employeeID, ws.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO slack_user_map (team_id, slack_user_id, organization_id, employee_id)
			VALUES ($1, $2, $3::uuid, $4::uuid)
			ON CONFLICT (team_id, slack_user_id)
			DO UPDATE SET employee_id = EXCLUDED.employee_id, organization_id = EXCLUDED.organization_id`,
			teamID, slackUserID, ws.OrganizationID, employeeID)
		return err
	})
}

func (s *PostgresStore) GetSlackUserEmployee(ctx context.Context, teamID, slackUserID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ws, err := s.GetSlackWorkspace(ctx, teamID)
	if err != nil {
		return "", err
	}
	var empID string
	err = s.inTenant(ctx, ws.OrganizationID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT employee_id::text FROM slack_user_map
			 WHERE team_id = $1 AND slack_user_id = $2 AND organization_id = $3::uuid`,
			teamID, slackUserID, ws.OrganizationID).Scan(&empID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: slack user %s", ErrNotFound, slackUserID)
	}
	return empID, err
}

func (s *PostgresStore) UnmapSlackUser(ctx context.Context, teamID, slackUserID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ws, err := s.GetSlackWorkspace(ctx, teamID)
	if err != nil {
		return err
	}
	return s.inTenant(ctx, ws.OrganizationID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM slack_user_map WHERE team_id = $1 AND slack_user_id = $2 AND organization_id = $3::uuid`,
			teamID, slackUserID, ws.OrganizationID)
		return err
	})
}

// ---------------------------------------------------------------------------
// Teams configs
// ---------------------------------------------------------------------------

func (s *PostgresStore) SaveTeamsConfig(ctx context.Context, c TeamsConfig) (TeamsConfig, error) {
	if err := ctx.Err(); err != nil {
		return TeamsConfig{}, err
	}
	if err := c.Validate(); err != nil {
		return TeamsConfig{}, err
	}
	var out TeamsConfig
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
		err := tx.QueryRow(ctx, `
			UPDATE teams_configs
			SET tenant_ref = $2, enc_shared_secret = $3, version = version + 1
			WHERE organization_id = $1::uuid AND version = $4
			RETURNING `+teamsConfigCols,
			c.OrganizationID, c.TenantRef, c.EncSharedSecret, c.Version,
		).Scan(
			&out.OrganizationID, &out.TenantRef, &out.EncSharedSecret,
			&out.Version, &out.CreatedAt, &out.UpdatedAt,
		)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM teams_configs WHERE organization_id = $1::uuid)`,
			c.OrganizationID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%w: teams config %s", ErrVersionConflict, c.OrganizationID)
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO teams_configs (organization_id, tenant_ref, enc_shared_secret)
			VALUES ($1::uuid, $2, $3)
			RETURNING `+teamsConfigCols,
			c.OrganizationID, c.TenantRef, c.EncSharedSecret,
		).Scan(
			&out.OrganizationID, &out.TenantRef, &out.EncSharedSecret,
			&out.Version, &out.CreatedAt, &out.UpdatedAt,
		)
		return mapPgError(err, "teams config")
	})
	if err != nil {
		return TeamsConfig{}, err
	}
	return out, nil
}

func (s *PostgresStore) GetTeamsConfig(ctx context.Context, orgID string) (TeamsConfig, error) {
	if err := ctx.Err(); err != nil {
		return TeamsConfig{}, err
	}
	var out TeamsConfig
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var err error
		out, err = scanTeamsConfig(tx.QueryRow(ctx,
			`SELECT `+teamsConfigCols+` FROM teams_configs WHERE organization_id = $1::uuid`, orgID))
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: teams config %s", ErrNotFound, orgID)
		}
		return err
	})
	if err != nil {
		return TeamsConfig{}, err
	}
	return out, nil
}

// ListTeamsConfigs reads outside a tenant transaction (cross-tenant by
// nature: the inbound Teams webhook compares the presented ?key= against
// every config). Mirrors GetCredentialByTokenHash's direct-pool pattern.
func (s *PostgresStore) ListTeamsConfigs(ctx context.Context, p Page) ([]TeamsConfig, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	limit := normalizeLimit(p.Limit)
	rows, err := s.pool.Query(ctx,
		`SELECT `+teamsConfigCols+` FROM teams_configs ORDER BY created_at, organization_id LIMIT $1`,
		limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []TeamsConfig
	for rows.Next() {
		c, err := scanTeamsConfig(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	// Cursor pagination is overkill for the handful of Teams configs an
	// instance holds; honor the limit and report no further pages.
	if len(out) > limit {
		out = out[:limit]
	}
	return out, "", nil
}

func (s *PostgresStore) DeleteTeamsConfig(ctx context.Context, orgID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`DELETE FROM teams_configs WHERE organization_id = $1::uuid`, orgID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("%w: teams config %s", ErrNotFound, orgID)
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Teams user identity map
// ---------------------------------------------------------------------------

func (s *PostgresStore) MapTeamsUser(ctx context.Context, orgID, teamsUserID, employeeID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if teamsUserID == "" {
		return &ValidationError{Field: "teams_user_id", Message: "required"}
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid)`,
			employeeID, orgID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: employee %s", ErrNotFound, employeeID)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO teams_user_map (organization_id, teams_user_id, employee_id)
			VALUES ($1::uuid, $2, $3::uuid)
			ON CONFLICT (organization_id, teams_user_id)
			DO UPDATE SET employee_id = EXCLUDED.employee_id`,
			orgID, teamsUserID, employeeID)
		return err
	})
}

func (s *PostgresStore) GetTeamsUserEmployee(ctx context.Context, orgID, teamsUserID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var empID string
	err := s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT employee_id::text FROM teams_user_map
			 WHERE organization_id = $1::uuid AND teams_user_id = $2`,
			orgID, teamsUserID).Scan(&empID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: teams user %s", ErrNotFound, teamsUserID)
	}
	return empID, err
}

func (s *PostgresStore) UnmapTeamsUser(ctx context.Context, orgID, teamsUserID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.inTenant(ctx, orgID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM teams_user_map WHERE organization_id = $1::uuid AND teams_user_id = $2`,
			orgID, teamsUserID)
		return err
	})
}

// ---------------------------------------------------------------------------
// ESS magic-link tokens
// ---------------------------------------------------------------------------

func (s *PostgresStore) IssueESSToken(ctx context.Context, t ESSToken) (ESSToken, error) {
	if err := ctx.Err(); err != nil {
		return ESSToken{}, err
	}
	if err := t.Validate(); err != nil {
		return ESSToken{}, err
	}
	var out ESSToken
	err := s.inTenant(ctx, t.OrganizationID, func(tx pgx.Tx) error {
		var ok bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM organizations WHERE id = $1::uuid)`,
			t.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: organization %s", ErrNotFound, t.OrganizationID)
		}
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM employees WHERE id = $1::uuid AND organization_id = $2::uuid)`,
			t.EmployeeID, t.OrganizationID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: employee %s", ErrNotFound, t.EmployeeID)
		}
		var err error
		out, err = scanESSToken(tx.QueryRow(ctx, `
			INSERT INTO ess_tokens (token_hash, organization_id, employee_id, expires_at)
			VALUES ($1, $2::uuid, $3::uuid, $4)
			RETURNING `+essTokenCols,
			t.TokenHash, t.OrganizationID, t.EmployeeID, t.ExpiresAt))
		if err != nil {
			return mapPgError(err, "ess token")
		}
		return nil
	})
	if err != nil {
		return ESSToken{}, err
	}
	return out, nil
}

// GetESSToken looks a token up by its digest. Cross-tenant by nature (the
// digest arrives before the org is known); the digest itself is the lookup
// key, mirroring GetCredentialByTokenHash.
func (s *PostgresStore) GetESSToken(ctx context.Context, tokenHash string) (ESSToken, error) {
	if err := ctx.Err(); err != nil {
		return ESSToken{}, err
	}
	out, err := scanESSToken(s.pool.QueryRow(ctx,
		`SELECT `+essTokenCols+` FROM ess_tokens WHERE token_hash = $1`, tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return ESSToken{}, fmt.Errorf("%w: ess token", ErrNotFound)
	}
	return out, err
}

func (s *PostgresStore) MarkESSTokenUsed(ctx context.Context, tokenHash string, at time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Resolve the org first for tenant scoping.
	tok, err := s.GetESSToken(ctx, tokenHash)
	if err != nil {
		return err
	}
	return s.inTenant(ctx, tok.OrganizationID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE ess_tokens SET used_at = $2 WHERE token_hash = $1 AND used_at IS NULL`,
			tokenHash, at.UTC())
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			// Already used (or vanished between the two reads): single-use
			// semantics are preserved — the first redemption wins.
			var exists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS(SELECT 1 FROM ess_tokens WHERE token_hash = $1)`,
				tokenHash).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: ess token", ErrNotFound)
			}
		}
		return nil
	})
}

func (s *PostgresStore) DeleteExpiredESSTokens(ctx context.Context, before time.Time) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	ct, err := s.pool.Exec(ctx, `DELETE FROM ess_tokens WHERE expires_at < $1`, before.UTC())
	if err != nil {
		return 0, err
	}
	return ct.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Cross-org employee lookup (ESS login)
// ---------------------------------------------------------------------------

// FindEmployeeByEmail locates a joined employee by email across
// organizations. Cross-tenant by nature; mirrors GetCredentialByTokenHash's
// direct-pool pattern.
func (s *PostgresStore) FindEmployeeByEmail(ctx context.Context, email string) (string, Employee, error) {
	if err := ctx.Err(); err != nil {
		return "", Employee{}, err
	}
	want := email
	if want == "" {
		return "", Employee{}, &ValidationError{Field: "email", Message: "required"}
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+employeeCols+` FROM employees WHERE lower(email) = lower($1) AND status = 'joined' LIMIT 3`,
		want)
	if err != nil {
		return "", Employee{}, err
	}
	defer rows.Close()
	var emps []Employee
	for rows.Next() {
		e, err := scanEmployee(rows)
		if err != nil {
			return "", Employee{}, err
		}
		emps = append(emps, e)
	}
	if err := rows.Err(); err != nil {
		return "", Employee{}, err
	}
	switch len(emps) {
	case 0:
		return "", Employee{}, fmt.Errorf("%w: employee with email %q", ErrNotFound, email)
	case 1:
		return emps[0].OrganizationID, emps[0], nil
	default:
		return "", Employee{}, fmt.Errorf("store: email %q is ambiguous across organizations", email)
	}
}
