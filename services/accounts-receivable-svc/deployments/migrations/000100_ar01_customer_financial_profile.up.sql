-- Migration: 000100_ar01_customer_financial_profile.up.sql
-- AR-01 Customer Financial Profile (spec ZS-SVC-C-001 section 4).
--   ar01_financial_profiles       stable profile per (tenant, legal entity, customer party)
--   ar01_profile_versions         append-only attribute versions (credit limit, prefs, dunning, AR policy, risk)
--   ar01_payment_term_versions    effective-dated payment terms with explicit intervals; overlap impossible
--   ar01_credit_limit_requests    pending out-of-policy limit increases (maker-checker)
--   ar01_profile_history          append-only evidence of every command (prior/new, actor/approver, reason)

CREATE TABLE IF NOT EXISTS ar01_financial_profiles (
    profile_id              UUID PRIMARY KEY,
    tenant_id               UUID          NOT NULL,
    legal_entity_id         UUID          NOT NULL,
    customer_party_id       UUID          NOT NULL,
    state                   VARCHAR(16)   NOT NULL CHECK (state IN ('DRAFT','ACTIVE','ON_HOLD','SUSPENDED','RETIRED')),
    credit_hold             BOOLEAN       NOT NULL DEFAULT FALSE,
    hold_reason_code        VARCHAR(128),
    hold_placed_by          VARCHAR(255),
    hold_placed_at          TIMESTAMPTZ,
    credit_limit_policy_max NUMERIC(20,4) NOT NULL DEFAULT 0 CHECK (credit_limit_policy_max >= 0),
    created_by              VARCHAR(255)  NOT NULL,
    version                 INTEGER       NOT NULL DEFAULT 1 CHECK (version >= 1),
    created_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at              TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar01_hold_matches_state CHECK (credit_hold = (state = 'ON_HOLD'))
);
-- One live profile per customer per entity; a retired profile frees the slot.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar01_profile_live
    ON ar01_financial_profiles (tenant_id, legal_entity_id, customer_party_id) WHERE state <> 'RETIRED';
CREATE INDEX IF NOT EXISTS idx_ar01_profile_party
    ON ar01_financial_profiles (tenant_id, legal_entity_id, customer_party_id);

CREATE TABLE IF NOT EXISTS ar01_profile_versions (
    version_id            UUID PRIMARY KEY,
    tenant_id             UUID          NOT NULL,
    legal_entity_id       UUID          NOT NULL,
    profile_id            UUID          NOT NULL REFERENCES ar01_financial_profiles (profile_id),
    seq                   INTEGER       NOT NULL CHECK (seq >= 1),
    effective_from        DATE          NOT NULL,
    credit_limit          NUMERIC(20,4) NOT NULL CHECK (credit_limit >= 0),
    credit_currency       VARCHAR(3)    NOT NULL CHECK (credit_currency ~ '^[A-Z]{3}$'),
    invoice_preferences   JSONB         NOT NULL DEFAULT '{}'::jsonb,
    statement_preferences JSONB         NOT NULL DEFAULT '{}'::jsonb,
    dunning_profile       VARCHAR(64)   NOT NULL DEFAULT '',
    ar_account_policy     VARCHAR(64)   NOT NULL DEFAULT '',
    risk_attributes       JSONB         NOT NULL DEFAULT '{}'::jsonb,
    change_kind           VARCHAR(32)   NOT NULL CHECK (change_kind IN ('CREATE','AMEND','CREDIT_LIMIT')),
    reason_code           VARCHAR(128),
    evidence_id           VARCHAR(255),
    created_by            VARCHAR(255)  NOT NULL,
    approved_by           VARCHAR(255),
    approval_request_id   UUID,
    recorded_at           TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    UNIQUE (profile_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_ar01_versions_eff ON ar01_profile_versions (profile_id, effective_from DESC, seq DESC);

-- Versions are immutable and recorded in sequence, with non-decreasing effective dates.
CREATE OR REPLACE FUNCTION ar01_check_version_order() RETURNS trigger AS $$
DECLARE
    latest DATE;
    mx     INTEGER;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.profile_id::text, 100));
    SELECT max(effective_from), max(seq) INTO latest, mx FROM ar01_profile_versions WHERE profile_id = NEW.profile_id;
    IF mx IS NOT NULL AND NEW.seq <> mx + 1 THEN
        RAISE EXCEPTION 'AR01_VERSION_SEQUENCE: version seq % is not the next after %', NEW.seq, mx;
    END IF;
    IF mx IS NULL AND NEW.seq <> 1 THEN
        RAISE EXCEPTION 'AR01_VERSION_SEQUENCE: first version must be seq 1';
    END IF;
    IF latest IS NOT NULL AND NEW.effective_from < latest THEN
        RAISE EXCEPTION 'AR01_VERSION_OVERLAP: effective_from % precedes the latest recorded version (%)', NEW.effective_from, latest;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ar01_profile_versions_order ON ar01_profile_versions;
CREATE TRIGGER ar01_profile_versions_order BEFORE INSERT ON ar01_profile_versions
    FOR EACH ROW EXECUTE FUNCTION ar01_check_version_order();
DROP TRIGGER IF EXISTS ar01_profile_versions_append_only ON ar01_profile_versions;
CREATE TRIGGER ar01_profile_versions_append_only BEFORE UPDATE OR DELETE ON ar01_profile_versions
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

-- Payment terms carry an explicit half-open interval [effective_from, effective_to).
-- effective_to NULL = open-ended. Two versions of one profile can never overlap.
CREATE TABLE IF NOT EXISTS ar01_payment_term_versions (
    term_version_id UUID PRIMARY KEY,
    tenant_id       UUID         NOT NULL,
    legal_entity_id UUID         NOT NULL,
    profile_id      UUID         NOT NULL REFERENCES ar01_financial_profiles (profile_id),
    seq             INTEGER      NOT NULL CHECK (seq >= 1),
    payment_terms_code VARCHAR(32) NOT NULL CHECK (payment_terms_code ~ '^[A-Z0-9_]{1,32}$'),
    payment_terms_days INTEGER   NOT NULL CHECK (payment_terms_days BETWEEN 0 AND 365),
    effective_from  DATE         NOT NULL,
    effective_to    DATE,
    reason_code     VARCHAR(128),
    evidence_id     VARCHAR(255),
    created_by      VARCHAR(255) NOT NULL,
    recorded_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ar01_term_interval CHECK (effective_to IS NULL OR effective_to > effective_from),
    UNIQUE (profile_id, seq)
);
CREATE INDEX IF NOT EXISTS idx_ar01_terms_eff ON ar01_payment_term_versions (profile_id, effective_from);
-- At most one open-ended version per profile.
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar01_term_open ON ar01_payment_term_versions (profile_id) WHERE effective_to IS NULL;

CREATE OR REPLACE FUNCTION ar01_check_term_overlap() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'DELETE on % is not permitted: payment term versions are never removed', TG_TABLE_NAME;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        -- The only permitted change: closing an open interval at a later date.
        IF OLD.effective_to IS NOT NULL OR NEW.effective_to IS NULL OR NEW.effective_to <= OLD.effective_from
           OR NEW.term_version_id <> OLD.term_version_id OR NEW.profile_id <> OLD.profile_id
           OR NEW.payment_terms_code <> OLD.payment_terms_code OR NEW.payment_terms_days <> OLD.payment_terms_days
           OR NEW.effective_from <> OLD.effective_from OR NEW.seq <> OLD.seq OR NEW.created_by <> OLD.created_by THEN
            RAISE EXCEPTION 'AR01_PAYMENT_TERMS_IMMUTABLE: a payment term version may only have its open interval closed';
        END IF;
        PERFORM pg_advisory_xact_lock(hashtextextended(NEW.profile_id::text, 101));
        IF EXISTS (SELECT 1 FROM ar01_payment_term_versions
                   WHERE profile_id = NEW.profile_id AND term_version_id <> NEW.term_version_id
                     AND daterange(effective_from, effective_to, '[)') && daterange(NEW.effective_from, NEW.effective_to, '[)')) THEN
            RAISE EXCEPTION 'AR01_PAYMENT_TERMS_OVERLAP: closing interval would overlap another payment term version';
        END IF;
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended(NEW.profile_id::text, 101));
    IF EXISTS (SELECT 1 FROM ar01_payment_term_versions
               WHERE profile_id = NEW.profile_id
                 AND daterange(effective_from, effective_to, '[)') && daterange(NEW.effective_from, NEW.effective_to, '[)')) THEN
        RAISE EXCEPTION 'AR01_PAYMENT_TERMS_OVERLAP: payment term version from % overlaps an existing version', NEW.effective_from;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS ar01_term_versions_guard ON ar01_payment_term_versions;
CREATE TRIGGER ar01_term_versions_guard BEFORE INSERT OR UPDATE OR DELETE ON ar01_payment_term_versions
    FOR EACH ROW EXECUTE FUNCTION ar01_check_term_overlap();

CREATE TABLE IF NOT EXISTS ar01_credit_limit_requests (
    request_id            UUID PRIMARY KEY,
    tenant_id             UUID          NOT NULL,
    legal_entity_id       UUID          NOT NULL,
    profile_id            UUID          NOT NULL REFERENCES ar01_financial_profiles (profile_id),
    requested_limit       NUMERIC(20,4) NOT NULL CHECK (requested_limit >= 0),
    currency              VARCHAR(3)    NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    prior_limit           NUMERIC(20,4) NOT NULL CHECK (prior_limit >= 0),
    policy_max_at_request NUMERIC(20,4) NOT NULL,
    requested_by          VARCHAR(255)  NOT NULL,
    reason_code           VARCHAR(128)  NOT NULL,
    evidence_id           VARCHAR(255),
    status                VARCHAR(16)   NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','APPROVED','REJECTED')),
    decided_by            VARCHAR(255),
    decided_at            TIMESTAMPTZ,
    decision_reason       VARCHAR(128),
    created_at            TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT ar01_request_decided CHECK (status = 'PENDING' OR decided_by IS NOT NULL),
    -- Segregation of duties enforced in the data too: nobody approves their own request.
    CONSTRAINT ar01_request_sod CHECK (status <> 'APPROVED' OR decided_by <> requested_by)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_ar01_request_pending ON ar01_credit_limit_requests (profile_id) WHERE status = 'PENDING';

CREATE TABLE IF NOT EXISTS ar01_profile_history (
    history_id         UUID PRIMARY KEY,
    tenant_id          UUID         NOT NULL,
    legal_entity_id    UUID         NOT NULL,
    profile_id         UUID         NOT NULL REFERENCES ar01_financial_profiles (profile_id),
    profile_version    INTEGER      NOT NULL,
    command            VARCHAR(64)  NOT NULL,
    state_after        VARCHAR(16)  NOT NULL,
    credit_hold_after  BOOLEAN      NOT NULL,
    actor_id           VARCHAR(255) NOT NULL,
    approver_id        VARCHAR(255),
    reason_code        VARCHAR(128),
    evidence_id        VARCHAR(255),
    effective_from     DATE,
    prior_values       JSONB,
    new_values         JSONB,
    policy_ref         VARCHAR(255),
    correlation_id     VARCHAR(255) NOT NULL DEFAULT '',
    recorded_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    UNIQUE (profile_id, profile_version)
);
DROP TRIGGER IF EXISTS ar01_profile_history_append_only ON ar01_profile_history;
CREATE TRIGGER ar01_profile_history_append_only BEFORE UPDATE OR DELETE ON ar01_profile_history
    FOR EACH ROW EXECUTE FUNCTION ar_forbid_mutation();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ar01_financial_profiles','ar01_profile_versions','ar01_payment_term_versions',
                             'ar01_credit_limit_requests','ar01_profile_history'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation_policy ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::UUID)$p$, t);
    END LOOP;
END $$;
