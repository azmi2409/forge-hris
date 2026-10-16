-- forge-hris component 6: chat integrations + ESS schema (Postgres 14+).
--
-- Slack workspace connections, Slack/Teams user identity maps, Teams
-- tenant configs, and ESS magic-link tokens.
--
-- Security conventions:
--   * Secrets are NEVER stored in plaintext: enc_signing_secret,
--     enc_service_token, and enc_shared_secret are AES-GCM envelopes
--     produced by the integrations/secrets package (key from
--     FORGE_HRIS_SECRET_KEY). The columns are opaque TEXT by design.
--   * ess_tokens stores only the SHA-256 hex digest of the magic link;
--     the plaintext token is shown once at issuance and never persisted.
--   * Versioned tables (slack_workspaces, teams_configs) carry a version
--     column: updates match on (key, version) so concurrent agent writes
--     fail fast with 0 rows and the adapter maps that to
--     ErrVersionConflict.
--   * Row-level security: every tenant table carries organization_id and a
--     tenant_isolation policy keyed on app.tenant_id (see 001_init.sql).
--     slack_user_map carries organization_id (in addition to its natural
--     (team_id, slack_user_id) PK) purely so the tenant policy can apply.

CREATE TABLE IF NOT EXISTS slack_workspaces (
    team_id            TEXT PRIMARY KEY,
    organization_id    UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    team_name          TEXT NOT NULL DEFAULT '',
    enc_signing_secret TEXT NOT NULL, -- AES-GCM envelope; never plaintext
    enc_service_token  TEXT NOT NULL, -- AES-GCM envelope; never plaintext
    version            BIGINT NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_slack_workspaces_org ON slack_workspaces (organization_id);

CREATE TABLE IF NOT EXISTS slack_user_map (
    team_id         TEXT NOT NULL REFERENCES slack_workspaces(team_id) ON DELETE CASCADE,
    slack_user_id   TEXT NOT NULL,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, slack_user_id)
);
CREATE INDEX IF NOT EXISTS idx_slack_user_map_employee ON slack_user_map (organization_id, employee_id);

CREATE TABLE IF NOT EXISTS teams_configs (
    organization_id   UUID PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    tenant_ref        TEXT NOT NULL DEFAULT '',
    enc_shared_secret TEXT NOT NULL, -- AES-GCM envelope; never plaintext
    version           BIGINT NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS teams_user_map (
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    teams_user_id   TEXT NOT NULL,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, teams_user_id)
);
CREATE INDEX IF NOT EXISTS idx_teams_user_map_employee ON teams_user_map (organization_id, employee_id);

CREATE TABLE IF NOT EXISTS ess_tokens (
    token_hash      TEXT PRIMARY KEY, -- hex(sha256(token)); plaintext never stored
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ess_tokens_employee ON ess_tokens (organization_id, employee_id);
CREATE INDEX IF NOT EXISTS idx_ess_tokens_expires ON ess_tokens (expires_at);

-- Keep updated_at fresh on the versioned tables (set_updated_at() is
-- defined in 001_init.sql).
DROP TRIGGER IF EXISTS trg_slack_workspaces_updated_at ON slack_workspaces;
CREATE TRIGGER trg_slack_workspaces_updated_at
    BEFORE UPDATE ON slack_workspaces
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_teams_configs_updated_at ON teams_configs;
CREATE TRIGGER trg_teams_configs_updated_at
    BEFORE UPDATE ON teams_configs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Row-level security: tenant isolation on every new table.
ALTER TABLE slack_workspaces ENABLE ROW LEVEL SECURITY;
ALTER TABLE slack_user_map ENABLE ROW LEVEL SECURITY;
ALTER TABLE teams_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE teams_user_map ENABLE ROW LEVEL SECURITY;
ALTER TABLE ess_tokens ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON slack_workspaces;
CREATE POLICY tenant_isolation ON slack_workspaces
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON slack_user_map;
CREATE POLICY tenant_isolation ON slack_user_map
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON teams_configs;
CREATE POLICY tenant_isolation ON teams_configs
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON teams_user_map;
CREATE POLICY tenant_isolation ON teams_user_map
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON ess_tokens;
CREATE POLICY tenant_isolation ON ess_tokens
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
