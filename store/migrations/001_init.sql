-- forge-hris component 1: core platform schema (Postgres 14+).
--
-- High-load design notes:
--   * Every versioned table (organizations, groups, employees, credentials)
--     carries a version column for optimistic concurrency: updates match on
--     (id, version) so concurrent agent writes fail fast with 0 rows and the
--     adapter maps that to ErrVersionConflict.
--   * Tenancy is enforced by row-level security: every tenant table carries
--     organization_id and a tenant_isolation policy keyed on the
--     app.tenant_id setting. The adapter runs each org-scoped operation in a
--     transaction that first executes SELECT set_config('app.tenant_id', $1,
--     true), and also keeps explicit organization_id = $1 predicates on every
--     query (belt and suspenders: RLS is the backstop, not the only filter).
--     organizations itself is tenant-scoped by id so global lookups
--     (by slug) work with the setting unset.
--   * audit_events is append-only: (organization_id, id) is unique and rows
--     are never updated or deleted. Hash chaining is computed by the
--     application (see store.computeAuditHash); the table just stores the
--     chain links.
--   * Employees are never hard-deleted: deactivation flips status to
--     'removed' so audit history stays referentially intact.

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS organizations (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT NOT NULL,
    slug         TEXT NOT NULL UNIQUE,
    country_code TEXT NOT NULL CHECK (country_code IN ('MY', 'ID')),
    version      BIGINT NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS groups (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    parent_id       UUID REFERENCES groups(id) ON DELETE SET NULL,
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_groups_org ON groups (organization_id);

CREATE TABLE IF NOT EXISTS employees (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_number TEXT NOT NULL,
    first_name      TEXT NOT NULL,
    last_name       TEXT NOT NULL DEFAULT '',
    email           TEXT NOT NULL DEFAULT '',
    position        TEXT NOT NULL DEFAULT '',
    employment_type TEXT NOT NULL DEFAULT 'full_time'
        CHECK (employment_type IN ('full_time', 'part_time', 'contract', 'intern')),
    group_id        UUID REFERENCES groups(id) ON DELETE SET NULL,
    manager_id      UUID REFERENCES employees(id) ON DELETE SET NULL,
    time_zone       TEXT NOT NULL DEFAULT '',
    work_start_date TIMESTAMPTZ,
    status          TEXT NOT NULL DEFAULT 'not_invited'
        CHECK (status IN ('not_invited', 'pending_invitation', 'joined', 'removed')),
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, employee_number)
);
-- Email uniqueness only applies when an email is set; many employees may
-- have none.
CREATE UNIQUE INDEX IF NOT EXISTS idx_employees_org_email
    ON employees (organization_id, email) WHERE email <> '';
CREATE INDEX IF NOT EXISTS idx_employees_org ON employees (organization_id);
CREATE INDEX IF NOT EXISTS idx_employees_group ON employees (group_id) WHERE group_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS memberships (
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'manager', 'member')),
    granted_by      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, employee_id)
);

CREATE TABLE IF NOT EXISTS credentials (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('pat', 'service_account')),
    name            TEXT NOT NULL,
    token_hash      TEXT NOT NULL UNIQUE, -- hex(sha256(plaintext)); plaintext never stored
    employee_id     UUID REFERENCES employees(id) ON DELETE SET NULL,
    scopes          TEXT[] NOT NULL DEFAULT '{}',
    expires_at      TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ,
    last_used_at    TIMESTAMPTZ,
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_credentials_org ON credentials (organization_id);

CREATE TABLE IF NOT EXISTS audit_events (
    id              BIGSERIAL PRIMARY KEY,
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    actor_type      TEXT NOT NULL CHECK (actor_type IN ('human', 'agent', 'system', 'scim')),
    actor_id        TEXT NOT NULL,
    on_behalf_of    TEXT,
    action          TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT,
    before          JSONB,
    after           JSONB,
    idempotency_key TEXT,
    prev_hash       TEXT NOT NULL DEFAULT '',
    hash            TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, id)
);
CREATE INDEX IF NOT EXISTS idx_audit_events_org
    ON audit_events (organization_id, id DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_action
    ON audit_events (organization_id, action);

CREATE TABLE IF NOT EXISTS idempotency_records (
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    key             TEXT NOT NULL,
    fingerprint     TEXT NOT NULL DEFAULT '',
    status_code     INT NOT NULL DEFAULT 0,
    response_body   BYTEA NOT NULL DEFAULT '\x',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (organization_id, key)
);

-- Keep updated_at fresh without application code.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_organizations_updated_at ON organizations;
CREATE TRIGGER trg_organizations_updated_at
    BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_groups_updated_at ON groups;
CREATE TRIGGER trg_groups_updated_at
    BEFORE UPDATE ON groups
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_employees_updated_at ON employees;
CREATE TRIGGER trg_employees_updated_at
    BEFORE UPDATE ON employees
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_credentials_updated_at ON credentials;
CREATE TRIGGER trg_credentials_updated_at
    BEFORE UPDATE ON credentials
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Row-level security: tenant isolation on every table.
ALTER TABLE organizations       ENABLE ROW LEVEL SECURITY;
ALTER TABLE groups              ENABLE ROW LEVEL SECURITY;
ALTER TABLE employees           ENABLE ROW LEVEL SECURITY;
ALTER TABLE memberships         ENABLE ROW LEVEL SECURITY;
ALTER TABLE credentials         ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events        ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_records ENABLE ROW LEVEL SECURITY;

-- organizations is scoped by its own id so global lookups (GetOrganization,
-- GetOrganizationBySlug, ListOrganizations) keep working with app.tenant_id
-- unset, while tenant-scoped operations see exactly one row.
DROP POLICY IF EXISTS tenant_isolation ON organizations;
CREATE POLICY tenant_isolation ON organizations
    USING (current_setting('app.tenant_id', true) = ''
        OR id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON groups;
CREATE POLICY tenant_isolation ON groups
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON employees;
CREATE POLICY tenant_isolation ON employees
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON memberships;
CREATE POLICY tenant_isolation ON memberships
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON credentials;
CREATE POLICY tenant_isolation ON credentials
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON audit_events;
CREATE POLICY tenant_isolation ON audit_events
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON idempotency_records;
CREATE POLICY tenant_isolation ON idempotency_records
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
