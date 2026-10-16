-- forge-hris component 4: webhooks schema (Postgres 14+).
--
-- Outbound HMAC webhooks (REA §5.4): tenant subscriptions plus a durable
-- outbox the dispatcher drains. Delivery is at-least-once: consumers must
-- dedupe on webhook_outbox.id.
--
-- Conventions mirror 003_leave.sql:
--   * Row-level security: every tenant table carries organization_id and a
--     tenant_isolation policy keyed on app.tenant_id (see 001_init.sql).
--   * webhook_outbox.status is pending | sent | dead; the claim index on
--     (status, next_attempt_at) serves the dispatcher's poll.

CREATE TABLE IF NOT EXISTS webhook_subscriptions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    url             TEXT NOT NULL,
    secret          TEXT NOT NULL, -- plaintext (see store/webhooks.go for the tradeoff note)
    events          TEXT[] NOT NULL DEFAULT '{}',
    active          BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_webhook_subscriptions_org ON webhook_subscriptions (organization_id);

CREATE TABLE IF NOT EXISTS webhook_outbox (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id UUID NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
    subscription_id UUID NOT NULL REFERENCES webhook_subscriptions(id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL,
    payload         JSONB NOT NULL,
    status          TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'sent', 'dead')),
    attempts        INT NOT NULL DEFAULT 0
        CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_webhook_outbox_org ON webhook_outbox (organization_id);
CREATE INDEX IF NOT EXISTS idx_webhook_outbox_claim ON webhook_outbox (status, next_attempt_at);
CREATE INDEX IF NOT EXISTS idx_webhook_outbox_subscription ON webhook_outbox (subscription_id);

-- Row-level security: tenant isolation on both tables.
ALTER TABLE webhook_subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_outbox        ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON webhook_subscriptions;
CREATE POLICY tenant_isolation ON webhook_subscriptions
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

DROP POLICY IF EXISTS tenant_isolation ON webhook_outbox;
CREATE POLICY tenant_isolation ON webhook_outbox
    USING (organization_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
