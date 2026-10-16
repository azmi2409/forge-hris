-- forge-hris component 5: payroll schema (Postgres 14+).
--
-- Payroll runs (Draft -> Approved -> Locked, with Cancelled as a terminal
-- side state), per-employee pay lines with itemized JSONB detail, org-level
-- item-preset overrides, sync items ingested from external sources, and
-- preview payloads with an expiry.
--
-- Conventions mirror 001_init.sql / 003_leave.sql:
--   * Versioned tables (payroll_runs, employee_pay_lines,
--     payroll_item_presets) carry a version column: updates match on
--     (id, version) so concurrent agent writes fail fast with 0 rows and the
--     adapter maps that to ErrVersionConflict.
--   * Money is int64 minor units (sen/cents) throughout: *_minor columns.
--   * Global presets are NOT a table: they live in code (the payroll
--     package's SeedPresets). payroll_item_presets holds org-level rows
--     only; their codes must not collide with seed codes (enforced in the
--     store, not the DB).
--   * Row-level security: every tenant table carries organization_id and a
--     tenant_isolation policy keyed on app.tenant_id (see 001_init.sql).

CREATE TABLE IF NOT EXISTS payroll_runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    period          TEXT NOT NULL, -- YYYY-MM
    country_code    TEXT NOT NULL CHECK (country_code IN ('MY', 'ID')),
    status          TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'approved', 'locked', 'cancelled')),
    rule_pack_version TEXT,
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, period)
);
CREATE INDEX IF NOT EXISTS idx_payroll_runs_org ON payroll_runs (organization_id);
CREATE INDEX IF NOT EXISTS idx_payroll_runs_status ON payroll_runs (organization_id, status);

CREATE TABLE IF NOT EXISTS employee_pay_lines (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id  UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    run_id           UUID NOT NULL REFERENCES payroll_runs(id) ON DELETE CASCADE,
    employee_id      UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    gross_minor      BIGINT NOT NULL DEFAULT 0,
    additions_minor  BIGINT NOT NULL DEFAULT 0,
    deductions_minor BIGINT NOT NULL DEFAULT 0,
    net_minor        BIGINT NOT NULL DEFAULT 0,
    items            JSONB, -- []payroll.PayLineItem
    statutory        JSONB, -- payroll.StatutorySplits
    bank_account     TEXT NOT NULL DEFAULT '',
    bank_code        TEXT NOT NULL DEFAULT '',
    version          BIGINT NOT NULL DEFAULT 1,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, employee_id)
);
CREATE INDEX IF NOT EXISTS idx_employee_pay_lines_run ON employee_pay_lines (run_id);
CREATE INDEX IF NOT EXISTS idx_employee_pay_lines_org ON employee_pay_lines (organization_id);

CREATE TABLE IF NOT EXISTS payroll_item_presets (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id          UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    code                     TEXT NOT NULL,
    name                     TEXT NOT NULL,
    kind                     TEXT NOT NULL
        CHECK (kind IN ('addition', 'deduction', 'employer_contribution')),
    subject_to_epf           BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_socso         BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_eis           BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_pcb           BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_bpjs_kes      BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_jht           BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_jkk           BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_jkm           BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_jp            BOOLEAN NOT NULL DEFAULT FALSE,
    subject_to_pph21         BOOLEAN NOT NULL DEFAULT FALSE,
    reduces_contribution_wages BOOLEAN NOT NULL DEFAULT FALSE,
    version                  BIGINT NOT NULL DEFAULT 1,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, code)
);
CREATE INDEX IF NOT EXISTS idx_payroll_item_presets_org ON payroll_item_presets (organization_id);

CREATE TABLE IF NOT EXISTS payroll_sync_items (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    run_id        UUID NOT NULL REFERENCES payroll_runs(id) ON DELETE CASCADE,
    employee_id   UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    source        TEXT NOT NULL DEFAULT '',
    external_id   TEXT NOT NULL DEFAULT '',
    label         TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL CHECK (kind IN ('addition', 'deduction')),
    preset_code   TEXT NOT NULL DEFAULT '',
    amount_minor  BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_payroll_sync_items_run ON payroll_sync_items (run_id);
CREATE INDEX IF NOT EXISTS idx_payroll_sync_items_org ON payroll_sync_items (organization_id);

CREATE TABLE IF NOT EXISTS payroll_previews (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    run_id        UUID NOT NULL REFERENCES payroll_runs(id) ON DELETE CASCADE,
    payload       JSONB,
    expires_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_payroll_previews_run ON payroll_previews (run_id);

-- Keep updated_at fresh on the versioned tables (set_updated_at() is
-- defined in 001_init.sql).
DROP TRIGGER IF EXISTS trg_payroll_runs_updated_at ON payroll_runs;
CREATE TRIGGER trg_payroll_runs_updated_at
    BEFORE UPDATE ON payroll_runs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_employee_pay_lines_updated_at ON employee_pay_lines;
CREATE TRIGGER trg_employee_pay_lines_updated_at
    BEFORE UPDATE ON employee_pay_lines
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_payroll_item_presets_updated_at ON payroll_item_presets;
CREATE TRIGGER trg_payroll_item_presets_updated_at
    BEFORE UPDATE ON payroll_item_presets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Row-level security: tenant isolation on every new table.
ALTER TABLE payroll_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE employee_pay_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_item_presets ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_sync_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_previews ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON payroll_runs;
CREATE POLICY tenant_isolation ON payroll_runs
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON employee_pay_lines;
CREATE POLICY tenant_isolation ON employee_pay_lines
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON payroll_item_presets;
CREATE POLICY tenant_isolation ON payroll_item_presets
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON payroll_sync_items;
CREATE POLICY tenant_isolation ON payroll_sync_items
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON payroll_previews;
CREATE POLICY tenant_isolation ON payroll_previews
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
