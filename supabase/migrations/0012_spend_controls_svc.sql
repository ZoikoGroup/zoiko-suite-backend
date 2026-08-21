-- 0012_spend_controls_svc.sql
-- spend-controls-svc → schema `spend_controls`
--
-- End state of 000001_initial_schema (the service's only migration).
-- Two tables: spend_policies, spend_consumptions.
--
-- ── The cross-currency comparison, made unrepresentable ──────────────────────
-- A consumption carries its own currency_code and so does the policy it is
-- recorded against, and nothing tied the two together: a 5,000 JPY spend could
-- be checked against a 5,000 GBP threshold and reported ALLOWED. Comparing two
-- amounts in different currencies is not a threshold check, it is a coincidence
-- of digits.
--
-- The composite foreign key below makes that shape impossible to store: a
-- consumption may only reference a policy whose currency is its own. It is a
-- backstop rather than the fix — the service should still refuse the request
-- with a clear error rather than a foreign-key violation — but the database no
-- longer holds a row asserting a comparison that was never valid.

CREATE SCHEMA IF NOT EXISTS spend_controls;

COMMENT ON SCHEMA spend_controls IS
    'spend-controls-svc. Spend thresholds by category and period, and the consumption ledger checked against them.';

GRANT USAGE ON SCHEMA spend_controls TO zoiko_backend, authenticated;

-- ── spend_policies ───────────────────────────────────────────────────────────

CREATE TABLE spend_controls.spend_policies (
    spend_policy_id         UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               VARCHAR(255)  NOT NULL,
    legal_entity_id         VARCHAR(255)  NOT NULL,

    category                VARCHAR(100)  NOT NULL,

    -- PER_TRANSACTION | MONTHLY | ANNUAL
    period                  VARCHAR(20)   NOT NULL,

    threshold_amount        NUMERIC(18,4) NOT NULL CHECK (threshold_amount > 0),
    currency_code           VARCHAR(10)   NOT NULL,

    active_flag             BOOLEAN       NOT NULL DEFAULT TRUE,

    created_by_principal_id VARCHAR(255)  NOT NULL DEFAULT app.current_principal_id(),
    created_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT spend_policies_period_known
        CHECK (period IN ('PER_TRANSACTION', 'MONTHLY', 'ANNUAL'))
);

CREATE INDEX idx_spend_policies_tenant_entity_category
    ON spend_controls.spend_policies (tenant_id, legal_entity_id, category);

-- Composite keys the consumption ledger's foreign key points at: one binds the
-- currency, the other binds the tenant. Both are needed to keep a consumption
-- from referencing a policy it has no business being measured against.
CREATE UNIQUE INDEX idx_spend_policies_id_currency
    ON spend_controls.spend_policies (spend_policy_id, currency_code);
CREATE UNIQUE INDEX idx_spend_policies_id_tenant
    ON spend_controls.spend_policies (spend_policy_id, tenant_id);

-- ── spend_consumptions ───────────────────────────────────────────────────────

CREATE TABLE spend_controls.spend_consumptions (
    consumption_id           UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                VARCHAR(255)  NOT NULL,
    legal_entity_id          VARCHAR(255)  NOT NULL,

    spend_policy_id          UUID          NOT NULL,

    amount                   NUMERIC(18,4) NOT NULL CHECK (amount > 0),
    currency_code            VARCHAR(10)   NOT NULL,

    source_reference         VARCHAR(255),
    correlation_id           VARCHAR(255)  NOT NULL,

    -- ALLOWED | BLOCKED
    decision_outcome         VARCHAR(20)   NOT NULL,

    recorded_by_principal_id VARCHAR(255)  NOT NULL DEFAULT app.current_principal_id(),
    recorded_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),

    CONSTRAINT spend_consumptions_outcome_known
        CHECK (decision_outcome IN ('ALLOWED', 'BLOCKED')),

    -- A consumption may only be recorded against a policy in the SAME currency.
    CONSTRAINT spend_consumptions_policy_currency_fk
        FOREIGN KEY (spend_policy_id, currency_code)
        REFERENCES spend_controls.spend_policies (spend_policy_id, currency_code)
        ON DELETE CASCADE,

    -- ...and only against a policy belonging to the SAME tenant. The compose
    -- schema referenced the policy id alone and carried its own tenant_id, so
    -- a consumption could be booked against another tenant's threshold.
    CONSTRAINT spend_consumptions_policy_tenant_fk
        FOREIGN KEY (spend_policy_id, tenant_id)
        REFERENCES spend_controls.spend_policies (spend_policy_id, tenant_id)
        ON DELETE CASCADE
);

-- Idempotency: a retried spend-check resolves to the original consumption
-- record, never a duplicate — which would otherwise consume the budget twice.
CREATE UNIQUE INDEX idx_spend_consumptions_tenant_correlation
    ON spend_controls.spend_consumptions (tenant_id, correlation_id);

CREATE INDEX idx_spend_consumptions_tenant_policy
    ON spend_controls.spend_consumptions (tenant_id, spend_policy_id, recorded_at);

-- ── Row-level security ───────────────────────────────────────────────────────

ALTER TABLE spend_controls.spend_policies     ENABLE ROW LEVEL SECURITY;
ALTER TABLE spend_controls.spend_policies     FORCE  ROW LEVEL SECURITY;
ALTER TABLE spend_controls.spend_consumptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE spend_controls.spend_consumptions FORCE  ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON spend_controls.spend_policies
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON spend_controls.spend_policies
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_isolation ON spend_controls.spend_consumptions
    FOR ALL
    TO zoiko_backend
    USING      (tenant_id = app.current_tenant_id())
    WITH CHECK (tenant_id = app.current_tenant_id());

CREATE POLICY tenant_read ON spend_controls.spend_consumptions
    FOR SELECT
    TO authenticated
    USING (tenant_id = app.current_tenant_id());

-- ── Grants ───────────────────────────────────────────────────────────────────

GRANT SELECT ON spend_controls.spend_policies     TO authenticated;
GRANT SELECT ON spend_controls.spend_consumptions TO authenticated;

GRANT SELECT, INSERT, UPDATE ON spend_controls.spend_policies TO zoiko_backend;

-- The consumption ledger is the record of what was checked and decided; a row
-- is never edited after the fact.
GRANT SELECT, INSERT ON spend_controls.spend_consumptions TO zoiko_backend;
