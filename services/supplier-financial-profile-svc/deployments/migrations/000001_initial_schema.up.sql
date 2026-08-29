-- 000001_initial_schema.up.sql
-- supplier-financial-profile-svc — initial schema (AP-01).
--
-- Four tables: supplier_financial_profiles is the one legitimately
-- mutable row (status/category/invoice_channel/payee_reference/
-- payment_method_preference all progress over the profile's life);
-- payment_terms_periods, high_risk_change_requests and
-- profile_change_events are append-only evidence, same doctrine as
-- every evidence table already built in this platform.
--
-- payee_reference has NO foreign key: AP-01's own contract names
-- "ORG-10 Banking Identity/Payee Master" as the authoritative owner of
-- the real payee/banking identity, and no such service exists anywhere
-- in this codebase. This column is an opaque reference this service
-- records and versions, never resolves — see internal/domain's package
-- doc comment.
CREATE TABLE supplier_financial_profiles (
    profile_id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID,

    legal_entity_id            TEXT        NOT NULL,
    supplier_ref               TEXT        NOT NULL, -- opaque party/role ID from an external party master
    -- DRAFT | ACTIVE | ON_HOLD | SUSPENDED | RETIRED.
    status                     VARCHAR(16) NOT NULL DEFAULT 'DRAFT',
    payee_reference            TEXT,       -- opaque ORG-10 reference — see package doc comment
    category                   TEXT,
    invoice_channel            TEXT,
    payment_method_preference  TEXT,
    tax_withholding_ref        TEXT,
    hold_reason                TEXT,

    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id    TEXT        NOT NULL,
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_supplier_financial_profiles_supplier ON supplier_financial_profiles (supplier_ref, legal_entity_id);

-- Requires btree_gist for the EXCLUDE constraint below (UUID equality +
-- range overlap in one constraint).
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE payment_terms_periods (
    payment_terms_id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID,
    profile_id                 UUID        NOT NULL REFERENCES supplier_financial_profiles(profile_id),

    terms_code                 VARCHAR(32) NOT NULL, -- data only, e.g. NET_30, NET_60, DUE_ON_RECEIPT
    effective_from             TIMESTAMPTZ NOT NULL,
    effective_to               TIMESTAMPTZ, -- NULL = open-ended

    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_by_principal_id    TEXT        NOT NULL,

    -- AP-01's own negative-path acceptance scenario #2: "supplier
    -- payment terms overlap effective periods" must be blocked, not
    -- silently reconciled. This EXCLUDE constraint makes that a genuine
    -- database invariant — two concurrent requests attempting to insert
    -- overlapping periods for the same profile cannot both succeed,
    -- which a "SELECT then INSERT if no overlap" application-level check
    -- cannot guarantee under concurrency. tstzrange's default bounds are
    -- [) — inclusive start, exclusive end — so a period ending exactly
    -- when another begins is correctly treated as non-overlapping.
    EXCLUDE USING gist (
        profile_id WITH =,
        tstzrange(effective_from, effective_to) WITH &&
    )
);

CREATE INDEX idx_payment_terms_periods_profile ON payment_terms_periods (profile_id, effective_from);

CREATE TABLE high_risk_change_requests (
    change_request_id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID,
    profile_id                 UUID        NOT NULL REFERENCES supplier_financial_profiles(profile_id),

    -- PAYEE_REFERENCE | PAYMENT_METHOD_PREFERENCE.
    field                       VARCHAR(32) NOT NULL,
    old_value                   TEXT,
    new_value                   TEXT        NOT NULL,
    reason                      TEXT,
    -- PENDING_APPROVAL | APPROVED | REJECTED.
    status                      VARCHAR(16) NOT NULL DEFAULT 'PENDING_APPROVAL',

    proposed_by_principal_id    TEXT        NOT NULL,
    proposed_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_by_principal_id     TEXT,
    decided_at                  TIMESTAMPTZ,
    decision_reason             TEXT
);

CREATE INDEX idx_high_risk_change_requests_profile ON high_risk_change_requests (profile_id, proposed_at DESC);

CREATE TABLE profile_change_events (
    event_id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID,
    profile_id                 UUID        NOT NULL REFERENCES supplier_financial_profiles(profile_id),

    event_type                 VARCHAR(64) NOT NULL, -- data only — see domain.Event* constants
    prior_value                TEXT,
    new_value                  TEXT,
    reason                     TEXT,
    actor_principal_id         TEXT        NOT NULL,

    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_profile_change_events_profile ON profile_change_events (profile_id, created_at ASC);
