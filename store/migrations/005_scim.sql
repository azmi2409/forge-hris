-- forge-hris component 4 (Worker C): SCIM 2.0 externalId mapping (Postgres 14+).
--
-- scim_links maps an IdP's SCIM externalId to one employee inside a tenant.
-- The mapping lives in its own table — NOT as a column on employees — so
-- SCIM concerns never leak into the core identity schema, and the link
-- survives the soft-deactivate lifecycle (employees are never hard-deleted),
-- letting an IdP re-correlate a re-provisioned user after DELETE
-- (active=false) without losing the mapping.
--
-- Conventions mirror 001_init.sql:
--   * Idempotent DDL (IF NOT EXISTS / DROP IF EXISTS throughout).
--   * Row-level security: tenant_isolation policy keyed on app.tenant_id
--     (see 001_init.sql); queries also carry explicit organization_id
--     predicates (belt and suspenders).
--   * employee_id UNIQUE: one employee carries at most one externalId.
--   * UNIQUE(organization_id, external_id): one externalId resolves to one
--     employee per tenant.

CREATE TABLE IF NOT EXISTS scim_links (
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    external_id     TEXT NOT NULL,
    employee_id     UUID NOT NULL UNIQUE REFERENCES employees(id) ON DELETE CASCADE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (organization_id, external_id)
);
CREATE INDEX IF NOT EXISTS idx_scim_links_org_external ON scim_links (organization_id, external_id);
CREATE INDEX IF NOT EXISTS idx_scim_links_employee ON scim_links (employee_id);

ALTER TABLE scim_links ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON scim_links;
CREATE POLICY tenant_isolation ON scim_links
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
