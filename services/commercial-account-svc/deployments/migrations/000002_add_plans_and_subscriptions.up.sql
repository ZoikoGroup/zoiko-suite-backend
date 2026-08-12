-- 000002_add_plans_and_subscriptions.up.sql
-- Chunk 6 — Plans, Pricing & Entitlements (docs/original_doc/zoiko_suite_doc7.txt
-- §B, §N1-N3, §U1). Extends commercial-account-svc rather than a new service:
-- a subscription is meaningless without the commercial_account it bills, and
-- entitlement resolution needs both in the same transaction boundary.

-- price_catalogs: one row per published catalog version. §U1 — an approved
-- catalog is never edited in place; a change is always a new version.
CREATE TABLE price_catalogs (
    catalog_version_id      UUID PRIMARY KEY,
    catalog_code             VARCHAR(64) NOT NULL,
    status                   VARCHAR(32) NOT NULL,
    effective_from           TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to             TIMESTAMP WITH TIME ZONE,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL
);

CREATE UNIQUE INDEX idx_price_catalogs_code_unique ON price_catalogs (catalog_code);

-- plans: display label + price is DATA per doc7 §B1 — this schema never
-- switches on plan_code. plan_id is the stable internal reference; catalog
-- callers key off plan_id, never display label (§B1 engineering rule).
CREATE TABLE plans (
    plan_id                  UUID PRIMARY KEY,
    catalog_version_id       UUID NOT NULL REFERENCES price_catalogs(catalog_version_id),
    plan_code                VARCHAR(64) NOT NULL,
    display_name             VARCHAR(255) NOT NULL,
    billing_interval         VARCHAR(32) NOT NULL,
    base_price_amount        NUMERIC(14,2) NOT NULL,
    base_price_currency_code VARCHAR(3) NOT NULL,
    market_scope             TEXT, -- comma-separated market codes; NULL = no market restriction recorded yet
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL
);

CREATE INDEX idx_plans_catalog_version ON plans (catalog_version_id);

-- entitlement_limits: the plan's entitlement_set (doc7 §28 lists
-- "price_catalog / entitlement_set" as one data-model line — modeled here as
-- a plan's owned limit rows rather than a separate detached object, since a
-- limit means nothing without the plan it entitles).
CREATE TABLE entitlement_limits (
    entitlement_limit_id     UUID PRIMARY KEY,
    plan_id                  UUID NOT NULL REFERENCES plans(plan_id),
    metric_type              VARCHAR(64) NOT NULL,
    limit_value               BIGINT, -- NULL = unlimited
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_entitlement_limits_plan_metric_unique ON entitlement_limits (plan_id, metric_type);

-- commercial_subscriptions: status values are doc7 §29's canonical
-- Commercial entitlement state machine verbatim — EVALUATION / ACTIVE /
-- PAST_DUE / RESTRICTED / SUSPENDED / CANCELED / TERMINATED.
CREATE TABLE commercial_subscriptions (
    subscription_id          UUID PRIMARY KEY,
    commercial_account_id    UUID NOT NULL REFERENCES commercial_accounts(commercial_account_id),
    plan_id                  UUID NOT NULL REFERENCES plans(plan_id),
    catalog_version_id       UUID NOT NULL REFERENCES price_catalogs(catalog_version_id),
    billing_interval         VARCHAR(32) NOT NULL,
    status                   VARCHAR(32) NOT NULL,
    renewal_date             TIMESTAMP WITH TIME ZONE,
    canceled_at              TIMESTAMP WITH TIME ZONE,
    processor_subscription_ref VARCHAR(255),
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL
);

CREATE INDEX idx_commercial_subscriptions_account ON commercial_subscriptions (commercial_account_id);

-- One non-terminal subscription per commercial account — doc7 doesn't
-- explicitly require this, but a commercial account with two simultaneously
-- ACTIVE subscriptions is exactly the double-billing risk §P3/§33 warn about.
CREATE UNIQUE INDEX idx_commercial_subscriptions_account_active_unique
    ON commercial_subscriptions (commercial_account_id)
    WHERE status NOT IN ('CANCELED', 'TERMINATED');

-- evaluation_programs: trial terms per doc7 §B3 — no free trial is assumed;
-- one must exist explicitly with duration/payment/conversion/expiry terms.
CREATE TABLE evaluation_programs (
    evaluation_program_id    UUID PRIMARY KEY,
    subscription_id          UUID NOT NULL REFERENCES commercial_subscriptions(subscription_id),
    duration_days            INT NOT NULL,
    payment_required         BOOLEAN NOT NULL DEFAULT FALSE,
    conversion_policy        VARCHAR(32) NOT NULL,
    expiry_action            VARCHAR(32) NOT NULL,
    started_at               TIMESTAMP WITH TIME ZONE NOT NULL,
    expires_at               TIMESTAMP WITH TIME ZONE NOT NULL,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL
);

CREATE UNIQUE INDEX idx_evaluation_programs_subscription_unique ON evaluation_programs (subscription_id);

-- contract_entitlement_overlays: doc7 §B6 — bespoke enterprise terms through
-- an approved overlay, never a hidden code switch. Overrides one metric's
-- limit for one commercial account, for a bounded effective period.
CREATE TABLE contract_entitlement_overlays (
    overlay_id                UUID PRIMARY KEY,
    commercial_account_id     UUID NOT NULL REFERENCES commercial_accounts(commercial_account_id),
    metric_type               VARCHAR(64) NOT NULL,
    override_limit_value      BIGINT,
    legal_reference           VARCHAR(255),
    effective_from            TIMESTAMP WITH TIME ZONE NOT NULL,
    effective_to              TIMESTAMP WITH TIME ZONE,
    approved_by_principal_id  VARCHAR(255) NOT NULL,
    created_at                TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    created_by_principal_id   VARCHAR(255) NOT NULL
);

CREATE INDEX idx_overlays_account_metric ON contract_entitlement_overlays (commercial_account_id, metric_type);

-- commercial_usage_meter_events: doc7 §B7/§N3 — "operational telemetry is
-- not commercial consent." usage_event_id is the caller-supplied dedupe key
-- (idempotency key) so a retried metering call can never double-count.
CREATE TABLE commercial_usage_meter_events (
    -- TEXT, not UUID: usage_event_id is a caller-supplied idempotency key
    -- (doc7 §L1) — callers are not required to mint a UUID for it, only to
    -- supply the SAME value on a retried metering call.
    usage_event_id            TEXT PRIMARY KEY,
    subscription_id           UUID NOT NULL REFERENCES commercial_subscriptions(subscription_id),
    metric_type               VARCHAR(64) NOT NULL,
    quantity                  NUMERIC(18,4) NOT NULL,
    occurred_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    source_service             VARCHAR(128) NOT NULL,
    billable_state              VARCHAR(32) NOT NULL,
    created_at                 TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_usage_events_subscription_metric ON commercial_usage_meter_events (subscription_id, metric_type);

-- subscription_change_requests: doc7 §B4-B5 upgrade/downgrade workflow —
-- persist the quote/preview before any entitlement actually changes, so an
-- upgrade is never applied without a prior, inspectable preview step.
CREATE TABLE subscription_change_requests (
    change_request_id         UUID PRIMARY KEY,
    subscription_id            UUID NOT NULL REFERENCES commercial_subscriptions(subscription_id),
    target_plan_id              UUID NOT NULL REFERENCES plans(plan_id),
    effective_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    status                      VARCHAR(32) NOT NULL,
    requested_by_principal_id   VARCHAR(255) NOT NULL,
    created_at                  TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    applied_at                  TIMESTAMP WITH TIME ZONE
);

CREATE INDEX idx_change_requests_subscription ON subscription_change_requests (subscription_id);
