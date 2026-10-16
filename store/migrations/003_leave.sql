-- forge-hris component 3: leave schema (Postgres 14+).
--
-- Time-off policies with an accrual engine (annually/monthly/pay_period/
-- payroll_hours), carry-forward caps and expiry; leave requests with frozen
-- snapshots; ordered multi-step approval chains; the annual leave-balance
-- ledger; holiday calendars + holidays.
--
-- Conventions mirror 001_init.sql:
--   * Versioned tables (time_off_policies, leave_balances, time_off_requests,
--     holiday_calendars) carry a version column: updates match on
--     (id, version) so concurrent agent writes fail fast with 0 rows and the
--     adapter maps that to ErrVersionConflict.
--   * Requests are never hard-deleted: cancellation flips status to
--     'cancelled' so the audit trail stays referentially intact.
--   * Row-level security: every tenant table carries organization_id and a
--     tenant_isolation policy keyed on app.tenant_id (see 001_init.sql).
--   * Money/payroll are untouched: the only payroll touchpoint is
--     time_off_requests.is_unpaid, consumed by component 5.

CREATE TABLE IF NOT EXISTS time_off_policies (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id   UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name              TEXT NOT NULL,
    accrual_mode      TEXT NOT NULL
        CHECK (accrual_mode IN ('annually', 'monthly', 'pay_period', 'payroll_hours')),
    accrual_amount    DOUBLE PRECISION NOT NULL DEFAULT 0
        CHECK (accrual_amount >= 0),
    carry_forward_cap DOUBLE PRECISION NOT NULL DEFAULT 0
        CHECK (carry_forward_cap >= 0),
    expiry_months     INT NOT NULL DEFAULT 0
        CHECK (expiry_months >= 0),
    version           BIGINT NOT NULL DEFAULT 1,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_time_off_policies_org ON time_off_policies (organization_id);

CREATE TABLE IF NOT EXISTS leave_balances (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id     UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    policy_id       UUID NOT NULL REFERENCES time_off_policies(id) ON DELETE CASCADE,
    period          TEXT NOT NULL, -- annual label, e.g. '2026'
    accrued         DOUBLE PRECISION NOT NULL DEFAULT 0,
    taken           DOUBLE PRECISION NOT NULL DEFAULT 0,
    carried         DOUBLE PRECISION NOT NULL DEFAULT 0,
    expired         DOUBLE PRECISION NOT NULL DEFAULT 0,
    balance         DOUBLE PRECISION NOT NULL DEFAULT 0,
    computed_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, employee_id, policy_id, period)
);
CREATE INDEX IF NOT EXISTS idx_leave_balances_org ON leave_balances (organization_id);
CREATE INDEX IF NOT EXISTS idx_leave_balances_employee ON leave_balances (organization_id, employee_id);

CREATE TABLE IF NOT EXISTS holiday_calendars (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    country_code    TEXT NOT NULL CHECK (country_code IN ('MY', 'ID')),
    version         BIGINT NOT NULL DEFAULT 1,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_holiday_calendars_org ON holiday_calendars (organization_id);

CREATE TABLE IF NOT EXISTS holidays (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    calendar_id     UUID NOT NULL REFERENCES holiday_calendars(id) ON DELETE CASCADE,
    date            DATE NOT NULL,
    name            TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (calendar_id, date)
);
CREATE INDEX IF NOT EXISTS idx_holidays_calendar ON holidays (calendar_id);
CREATE INDEX IF NOT EXISTS idx_holidays_org_date ON holidays (organization_id, date);

CREATE TABLE IF NOT EXISTS time_off_requests (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id     UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    employee_id         UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    policy_id           UUID NOT NULL REFERENCES time_off_policies(id) ON DELETE CASCADE,
    holiday_calendar_id UUID REFERENCES holiday_calendars(id) ON DELETE SET NULL,
    start_date          DATE NOT NULL,
    end_date            DATE NOT NULL,
    days                DOUBLE PRECISION NOT NULL DEFAULT 0,
    snapshot            JSONB,
    status              TEXT NOT NULL DEFAULT 'requested'
        CHECK (status IN ('requested', 'approved', 'rejected', 'cancelled')),
    is_unpaid           BOOLEAN NOT NULL DEFAULT FALSE,
    version             BIGINT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (end_date >= start_date)
);
CREATE INDEX IF NOT EXISTS idx_time_off_requests_org ON time_off_requests (organization_id);
CREATE INDEX IF NOT EXISTS idx_time_off_requests_employee ON time_off_requests (organization_id, employee_id);
CREATE INDEX IF NOT EXISTS idx_time_off_requests_status ON time_off_requests (organization_id, status);

CREATE TABLE IF NOT EXISTS time_off_approvals (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id      UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    request_id           UUID NOT NULL REFERENCES time_off_requests(id) ON DELETE CASCADE,
    sequence             INT NOT NULL CHECK (sequence >= 1),
    approver_employee_id UUID NOT NULL REFERENCES employees(id) ON DELETE CASCADE,
    status               TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected')),
    decided_by           TEXT,
    decided_at           TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (request_id, sequence)
);
CREATE INDEX IF NOT EXISTS idx_time_off_approvals_request ON time_off_approvals (request_id);

-- Keep updated_at fresh on the versioned tables (set_updated_at() is
-- defined in 001_init.sql).
DROP TRIGGER IF EXISTS trg_time_off_policies_updated_at ON time_off_policies;
CREATE TRIGGER trg_time_off_policies_updated_at
    BEFORE UPDATE ON time_off_policies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_leave_balances_updated_at ON leave_balances;
CREATE TRIGGER trg_leave_balances_updated_at
    BEFORE UPDATE ON leave_balances
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_time_off_requests_updated_at ON time_off_requests;
CREATE TRIGGER trg_time_off_requests_updated_at
    BEFORE UPDATE ON time_off_requests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_holiday_calendars_updated_at ON holiday_calendars;
CREATE TRIGGER trg_holiday_calendars_updated_at
    BEFORE UPDATE ON holiday_calendars
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Row-level security: tenant isolation on every new table.
ALTER TABLE time_off_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE leave_balances    ENABLE ROW LEVEL SECURITY;
ALTER TABLE time_off_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE time_off_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE holiday_calendars ENABLE ROW LEVEL SECURITY;
ALTER TABLE holidays          ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON time_off_policies;
CREATE POLICY tenant_isolation ON time_off_policies
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON leave_balances;
CREATE POLICY tenant_isolation ON leave_balances
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON time_off_requests;
CREATE POLICY tenant_isolation ON time_off_requests
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON time_off_approvals;
CREATE POLICY tenant_isolation ON time_off_approvals
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON holiday_calendars;
CREATE POLICY tenant_isolation ON holiday_calendars
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON holidays;
CREATE POLICY tenant_isolation ON holidays
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
