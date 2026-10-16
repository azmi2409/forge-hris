-- forge-hris component 2: time tracking schema (Postgres 14+).
--
-- Idempotent (IF NOT EXISTS / DROP IF EXISTS throughout); migrations apply
-- in filename order via store/migrate.go, so this runs after 001_init.sql.
--
-- Design notes:
--   * activity_id / location_id on time_entries are nullable UUIDs WITHOUT
--     foreign keys: activities and locations are forward references that
--     land in later components. project_id references projects (ON DELETE
--     SET NULL) so deleting a project never orphans a punch.
--   * prev_id / next_id form the correction chain (see store/time.go):
--     corrections freeze the original row (is_locked, next_id set) and add
--     a superseding head row with prev_id set. No FK cycles are possible:
--     next_id always points at a newer row.
--   * Pay periods freeze entries: approving a period sets is_locked on all
--     non-deleted entries it covers; reopening clears it. The API also
--     checks period status at write time (belt and suspenders).
--   * Row-level security: tenant_isolation on every tenant table, same
--     pattern as 001_init.sql.

CREATE TABLE IF NOT EXISTS projects (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    client_name     TEXT NOT NULL DEFAULT '',
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_projects_org ON projects (organization_id);

CREATE TABLE IF NOT EXISTS time_entries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('in', 'out', 'start_break', 'end_break')),
    occurred_at     TIMESTAMPTZ NOT NULL,
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    activity_id     UUID, -- forward ref: no FK until activities exist
    location_id     UUID, -- forward ref: no FK until locations exist
    latitude        DOUBLE PRECISION CHECK (latitude BETWEEN -90 AND 90),
    longitude       DOUBLE PRECISION CHECK (longitude BETWEEN -180 AND 180),
    address         TEXT,
    client_type     TEXT NOT NULL DEFAULT 'web'
        CHECK (client_type IN ('web', 'mobile', 'desktop', 'kiosk', 'ai_assistant')),
    prev_id         UUID REFERENCES time_entries(id),
    next_id         UUID REFERENCES time_entries(id),
    is_locked       BOOLEAN NOT NULL DEFAULT FALSE,
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_time_entries_employee
    ON time_entries (organization_id, employee_id, occurred_at DESC)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_time_entries_occurred
    ON time_entries (organization_id, occurred_at)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS hour_entries (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    work_date       DATE NOT NULL,
    hours           DOUBLE PRECISION NOT NULL CHECK (hours > 0),
    project_id      UUID REFERENCES projects(id) ON DELETE SET NULL,
    note            TEXT NOT NULL DEFAULT '',
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at      TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_hour_entries_employee
    ON hour_entries (organization_id, employee_id, work_date)
    WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS pay_period_definitions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    cadence         TEXT NOT NULL CHECK (cadence IN ('Monthly', 'TwiceInMonth', 'Weekly')),
    anchor_day      INT NOT NULL DEFAULT 1,
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_pay_period_definitions_org
    ON pay_period_definitions (organization_id);

CREATE TABLE IF NOT EXISTS pay_periods (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    definition_id   UUID NOT NULL REFERENCES pay_period_definitions(id) ON DELETE RESTRICT,
    period_start    TIMESTAMPTZ NOT NULL,
    period_end      TIMESTAMPTZ NOT NULL CHECK (period_end > period_start),
    status          TEXT NOT NULL DEFAULT 'Open'
        CHECK (status IN ('Open', 'Submitted', 'Approved', 'Rejected', 'Reopened')),
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (definition_id, period_start)
);
CREATE INDEX IF NOT EXISTS idx_pay_periods_org ON pay_periods (organization_id);
CREATE INDEX IF NOT EXISTS idx_pay_periods_range
    ON pay_periods (organization_id, period_start, period_end);

CREATE TABLE IF NOT EXISTS schedules (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id          UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    kind                 TEXT NOT NULL CHECK (kind IN ('Fixed', 'Flexible', 'Weekly')),
    expected_daily_hours DOUBLE PRECISION NOT NULL DEFAULT 8 CHECK (expected_daily_hours > 0),
    version              BIGINT NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_schedules_employee
    ON schedules (organization_id, employee_id);

CREATE TABLE IF NOT EXISTS shifts (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    shift_date      DATE NOT NULL,
    starts_at       TIMESTAMPTZ NOT NULL,
    ends_at         TIMESTAMPTZ NOT NULL CHECK (ends_at > starts_at),
    published       BOOLEAN NOT NULL DEFAULT FALSE,
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_shifts_employee_date
    ON shifts (organization_id, employee_id, shift_date);

-- Keep updated_at fresh without application code (set_updated_at() is
-- defined in 001_init.sql).
DROP TRIGGER IF EXISTS trg_projects_updated_at ON projects;
CREATE TRIGGER trg_projects_updated_at
    BEFORE UPDATE ON projects
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_time_entries_updated_at ON time_entries;
CREATE TRIGGER trg_time_entries_updated_at
    BEFORE UPDATE ON time_entries
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_hour_entries_updated_at ON hour_entries;
CREATE TRIGGER trg_hour_entries_updated_at
    BEFORE UPDATE ON hour_entries
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_pay_period_definitions_updated_at ON pay_period_definitions;
CREATE TRIGGER trg_pay_period_definitions_updated_at
    BEFORE UPDATE ON pay_period_definitions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_pay_periods_updated_at ON pay_periods;
CREATE TRIGGER trg_pay_periods_updated_at
    BEFORE UPDATE ON pay_periods
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_schedules_updated_at ON schedules;
CREATE TRIGGER trg_schedules_updated_at
    BEFORE UPDATE ON schedules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_shifts_updated_at ON shifts;
CREATE TRIGGER trg_shifts_updated_at
    BEFORE UPDATE ON shifts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Row-level security: tenant isolation on every table (same pattern as
-- 001_init.sql).
ALTER TABLE projects                ENABLE ROW LEVEL SECURITY;
ALTER TABLE time_entries            ENABLE ROW LEVEL SECURITY;
ALTER TABLE hour_entries            ENABLE ROW LEVEL SECURITY;
ALTER TABLE pay_period_definitions  ENABLE ROW LEVEL SECURITY;
ALTER TABLE pay_periods             ENABLE ROW LEVEL SECURITY;
ALTER TABLE schedules               ENABLE ROW LEVEL SECURITY;
ALTER TABLE shifts                  ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON projects;
CREATE POLICY tenant_isolation ON projects
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON time_entries;
CREATE POLICY tenant_isolation ON time_entries
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON hour_entries;
CREATE POLICY tenant_isolation ON hour_entries
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON pay_period_definitions;
CREATE POLICY tenant_isolation ON pay_period_definitions
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON pay_periods;
CREATE POLICY tenant_isolation ON pay_periods
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON schedules;
CREATE POLICY tenant_isolation ON schedules
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON shifts;
CREATE POLICY tenant_isolation ON shifts
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
