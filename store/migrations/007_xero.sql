-- forge-hris component 6: Xero integration schema (Postgres 14+).
--
-- OAuth connections (tokens stored as AES-256-GCM envelopes — see
-- integrations/secrets; the DB never sees plaintext), per-org
-- account/contact mappings, and the bill-posting ledger that makes posting
-- idempotent per (run_id, employee_id).
--
-- Conventions mirror 006_payroll.sql:
--   * xero_connections and xero_bill_postings carry a version column:
--     updates match on (id/organization_id, version) so concurrent agent
--     writes fail fast with 0 rows and the adapter maps that to
--     ErrVersionConflict.
--   * Row-level security: every tenant table carries organization_id and a
--     tenant_isolation policy keyed on app.tenant_id (see 001_init.sql).

CREATE TABLE IF NOT EXISTS xero_connections (
    organization_id   UUID PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
    tenant_id         TEXT NOT NULL DEFAULT '',
    enc_access_token  TEXT NOT NULL DEFAULT '',
    enc_refresh_token TEXT NOT NULL DEFAULT '',
    expires_at        TIMESTAMPTZ,
    auto_post         BOOLEAN NOT NULL DEFAULT FALSE,
    version           BIGINT NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS xero_account_mappings (
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    component_key   TEXT NOT NULL,
    account_code    TEXT NOT NULL,
    PRIMARY KEY (organization_id, component_key)
);

CREATE TABLE IF NOT EXISTS xero_contact_mappings (
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    contact_id      TEXT NOT NULL,
    PRIMARY KEY (organization_id, employee_id)
);
CREATE INDEX IF NOT EXISTS idx_xero_contact_mappings_employee ON xero_contact_mappings (employee_id);

CREATE TABLE IF NOT EXISTS xero_bill_postings (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id     UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    run_id              UUID NOT NULL REFERENCES payroll_runs(id) ON DELETE CASCADE,
    employee_id         UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    status              TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'posted', 'failed')),
    xero_invoice_id     TEXT NOT NULL DEFAULT '',
    xero_invoice_number TEXT NOT NULL DEFAULT '',
    error               TEXT NOT NULL DEFAULT '',
    idempotency_key     TEXT NOT NULL DEFAULT '',
    version             BIGINT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_xero_bill_postings_run ON xero_bill_postings (organization_id, run_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_xero_bill_postings_run_employee
    ON xero_bill_postings (organization_id, run_id, employee_id);

-- Keep updated_at fresh on the versioned connection table
-- (set_updated_at() is defined in 001_init.sql).
DROP TRIGGER IF EXISTS trg_xero_connections_updated_at ON xero_connections;
CREATE TRIGGER trg_xero_connections_updated_at
    BEFORE UPDATE ON xero_connections
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Row-level security: tenant isolation on every new table.
ALTER TABLE xero_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE xero_account_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE xero_contact_mappings ENABLE ROW LEVEL SECURITY;
ALTER TABLE xero_bill_postings ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON xero_connections;
CREATE POLICY tenant_isolation ON xero_connections
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON xero_account_mappings;
CREATE POLICY tenant_isolation ON xero_account_mappings
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON xero_contact_mappings;
CREATE POLICY tenant_isolation ON xero_contact_mappings
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON xero_bill_postings;
CREATE POLICY tenant_isolation ON xero_bill_postings
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
