-- 000007_add_com02_subscriptions.up.sql
-- COM-02 Subscription, part 2a (ZS-SVC-Q-001 §4.2; COM-CTRL-003, -006, -007).
--
-- ── Effective-dated, append-only ───────────────────────────────────────────
--
-- A subscription's commercial state is a sequence of immutable
-- subscription_versions, each effective from a server-decided instant. The
-- state at time t is the non-voided version with the latest effective_from
-- <= t. A scheduled change (a cancellation at term end, a trial converting)
-- is therefore a version that already exists with a future effective_from:
-- reads are correct at every instant without waiting for a background job,
-- and any past state can be reconstructed exactly (§2: "can be reconstructed
-- as-of a prior time").
--
-- A version that has not yet taken effect can be voided (a reactivation
-- voids the scheduled cancellation); a version that has taken effect never
-- changes. effective_to is derived from the next version, not stored, so
-- nothing is ever rewritten to "end-date" a predecessor.
--
-- Every version carries its full commercial configuration (items and
-- quantities bound to immutable price versions), so a version answers "what
-- had the customer agreed to at t" on its own.
--
-- ── Coexistence with the doc7 subscriptions ────────────────────────────────
--
-- commercial_subscriptions (migration 000002) stays for its existing rows.
-- An account may hold a live subscription in one model or the other, never
-- both: each side's INSERT trigger locks the account row and checks the
-- other. The lock serializes the two paths, so a concurrent insert on the
-- other table cannot slip past the check.
--
-- Custom SQLSTATEs: CP001 immutable row; CP002 account integrity; CP003 the
-- account already has a live subscription in the other model.

CREATE EXTENSION IF NOT EXISTS btree_gist;

-- ── Customer market (server-resolved context) ─────────────────────────────
--
-- The market a customer buys in is seller-side data, set by a ZoikoSuite
-- operator, never taken from the purchase request. tenant-entity-registry-svc
-- records data residency, which is a different fact.
ALTER TABLE commercial_accounts
    ADD COLUMN market_code VARCHAR(8) CHECK (market_code ~ '^[A-Z]{2,8}$'),
    ADD COLUMN market_set_at TIMESTAMPTZ,
    ADD COLUMN market_set_by_principal_id VARCHAR(255),
    ADD CONSTRAINT commercial_accounts_market_evidence CHECK (
        (market_code IS NULL) = (market_set_at IS NULL)
        AND (market_code IS NULL) = (market_set_by_principal_id IS NULL));

-- ── Subscriptions ─────────────────────────────────────────────────────────
CREATE TABLE subscriptions (
    subscription_id          TEXT         PRIMARY KEY
        CHECK (subscription_id ~ '^csub_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id          UUID         NOT NULL,
    commercial_account_id    UUID         NOT NULL REFERENCES commercial_accounts (commercial_account_id),
    product_id               TEXT         NOT NULL REFERENCES commercial_products (product_id),
    currency_code            CHAR(3)      NOT NULL REFERENCES commercial_currencies (currency_code),
    market_code              VARCHAR(8)   NOT NULL CHECK (market_code ~ '^[A-Z]{2,8}$'),
    billing_source           VARCHAR(32)  NOT NULL DEFAULT 'DIRECT' CHECK (billing_source IN ('DIRECT')),
    channel                  VARCHAR(16)  NOT NULL CHECK (channel IN ('SELF_SERVICE', 'ASSISTED')),
    customer_basis_ref       VARCHAR(255),
    payment_method_ref       VARCHAR(255),
    starts_at                TIMESTAMPTZ  NOT NULL,
    -- The instant the subscription stops: the effective_from of its scheduled
    -- or applied CANCELED/EXPIRED version. NULL while nothing ends it.
    ends_at                  TIMESTAMPTZ,
    row_version              INT          NOT NULL DEFAULT 1 CHECK (row_version >= 1),
    created_at               TIMESTAMPTZ  NOT NULL,
    created_by_principal_id  VARCHAR(255) NOT NULL,

    -- An assisted change records the operator AND the customer's basis for
    -- it (§4.2 Authorization/SoD).
    CONSTRAINT subscriptions_assisted_basis CHECK (
        channel <> 'ASSISTED' OR (customer_basis_ref IS NOT NULL AND btrim(customer_basis_ref) <> '')),
    CONSTRAINT subscriptions_end_after_start CHECK (ends_at IS NULL OR ends_at >= starts_at),
    -- One plan subscription per account at any instant. Two overlapping
    -- subscriptions would bill the same customer twice for the same period.
    CONSTRAINT subscriptions_no_overlap EXCLUDE USING gist (
        commercial_account_id WITH =,
        tstzrange(starts_at, ends_at, '[)') WITH &&)
);

CREATE INDEX idx_subscriptions_organization ON subscriptions (organization_id);

CREATE FUNCTION enforce_subscription_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    acct_org UUID;
BEGIN
    SELECT organization_id INTO acct_org
      FROM commercial_accounts
     WHERE commercial_account_id = NEW.commercial_account_id
       FOR UPDATE;
    IF acct_org IS NULL OR acct_org <> NEW.organization_id THEN
        RAISE EXCEPTION 'commercial account % does not belong to organization %',
            NEW.commercial_account_id, NEW.organization_id USING ERRCODE = 'CP002';
    END IF;
    IF EXISTS (SELECT 1 FROM commercial_subscriptions
                WHERE commercial_account_id = NEW.commercial_account_id
                  AND status NOT IN ('CANCELED', 'TERMINATED')) THEN
        RAISE EXCEPTION 'commercial account % already has a live doc7 subscription',
            NEW.commercial_account_id USING ERRCODE = 'CP003';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscriptions_insert
    BEFORE INSERT ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION enforce_subscription_insert();

-- The subscription row is the aggregate's lock and ETag; only ends_at moves,
-- and every accepted change advances row_version by exactly one.
CREATE FUNCTION enforce_subscription_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'subscription % cannot be deleted', OLD.subscription_id USING ERRCODE = 'CP001';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['ends_at', 'row_version']) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['ends_at', 'row_version']) THEN
        RAISE EXCEPTION 'subscription % identity is immutable; its state changes through versions', OLD.subscription_id
            USING ERRCODE = 'CP001';
    END IF;
    IF NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'subscription % row_version must advance by one', OLD.subscription_id USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscriptions_update
    BEFORE UPDATE OR DELETE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION enforce_subscription_update();

-- The doc7 side of the mutual exclusion. now() is correct here: the doc7
-- path has no effective dating, so "live" means live at insert time.
CREATE FUNCTION reject_legacy_subscription_over_com02() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM commercial_accounts WHERE commercial_account_id = NEW.commercial_account_id FOR UPDATE;
    IF EXISTS (SELECT 1 FROM subscriptions
                WHERE commercial_account_id = NEW.commercial_account_id
                  AND (ends_at IS NULL OR ends_at > now())) THEN
        RAISE EXCEPTION 'commercial account % already has a live COM-02 subscription',
            NEW.commercial_account_id USING ERRCODE = 'CP003';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_commercial_subscriptions_com02_exclusion
    BEFORE INSERT ON commercial_subscriptions
    FOR EACH ROW EXECUTE FUNCTION reject_legacy_subscription_over_com02();

-- ── Versions ──────────────────────────────────────────────────────────────
CREATE TABLE subscription_versions (
    subscription_version_id  TEXT         PRIMARY KEY
        CHECK (subscription_version_id ~ '^csv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    subscription_id          TEXT         NOT NULL REFERENCES subscriptions (subscription_id),
    version_number           INT          NOT NULL CHECK (version_number >= 1),
    lifecycle_status         VARCHAR(16)  NOT NULL
        CHECK (lifecycle_status IN ('TRIALING', 'PENDING', 'ACTIVE', 'CANCEL_PENDING', 'CANCELED', 'EXPIRED')),
    change_type              VARCHAR(32)  NOT NULL
        CHECK (change_type IN ('STARTED', 'TRIAL_CONVERSION', 'TRIAL_EXPIRY', 'ACTIVATED',
                               'CANCELLATION_SCHEDULED', 'CANCELLATION_EFFECTIVE', 'CANCELED_NOW',
                               'REACTIVATED', 'TERM_EXPIRY')),
    effective_from           TIMESTAMPTZ  NOT NULL,
    trial_ends_at            TIMESTAMPTZ,
    -- Every version written by one command shares its change_id: the
    -- "resulting commercial change ID" of §4.2 Evidence.
    change_id                TEXT         NOT NULL
        CHECK (change_id ~ '^cchg_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    reason                   TEXT,
    channel                  VARCHAR(16)  NOT NULL CHECK (channel IN ('SELF_SERVICE', 'ASSISTED')),
    customer_basis_ref       VARCHAR(255),
    created_at               TIMESTAMPTZ  NOT NULL,
    created_by_principal_id  VARCHAR(255) NOT NULL,
    voided_at                TIMESTAMPTZ,
    voided_by_principal_id   VARCHAR(255),
    void_reason              TEXT,
    voided_by_change_id      TEXT,

    CONSTRAINT subscription_versions_number_unique UNIQUE (subscription_id, version_number),
    CONSTRAINT subscription_versions_void_all_or_nothing CHECK (
        (voided_at IS NULL) = (voided_by_principal_id IS NULL)
        AND (voided_at IS NULL) = (void_reason IS NULL)
        AND (voided_at IS NULL) = (voided_by_change_id IS NULL)),
    -- Only a version that has not yet taken effect can be voided.
    CONSTRAINT subscription_versions_void_before_effect CHECK (voided_at IS NULL OR voided_at < effective_from),
    CONSTRAINT subscription_versions_assisted_basis CHECK (
        channel <> 'ASSISTED' OR (customer_basis_ref IS NOT NULL AND btrim(customer_basis_ref) <> '')),
    CONSTRAINT subscription_versions_trial_status CHECK (lifecycle_status <> 'TRIALING' OR trial_ends_at IS NOT NULL)
);

CREATE INDEX idx_subscription_versions_effective
    ON subscription_versions (subscription_id, effective_from DESC, version_number DESC)
    WHERE voided_at IS NULL;

-- ── Items and quantities ──────────────────────────────────────────────────
--
-- Each item is bound to an immutable price version and records the content
-- hash it was bound at: the price basis cannot drift under a subscription
-- (COM-CTRL-003), and the binding itself is evidence.
CREATE TABLE subscription_items (
    subscription_version_id  TEXT      NOT NULL REFERENCES subscription_versions (subscription_version_id),
    item_no                  INT       NOT NULL CHECK (item_no >= 1),
    item_role                VARCHAR(8) NOT NULL CHECK (item_role IN ('PLAN', 'ADD_ON')),
    price_version_id         TEXT      NOT NULL REFERENCES product_price_versions (price_version_id),
    price_content_sha256     CHAR(64)  NOT NULL CHECK (price_content_sha256 ~ '^[0-9a-f]{64}$'),
    accepted_terms_sha256    CHAR(64)  NOT NULL CHECK (accepted_terms_sha256 ~ '^[0-9a-f]{64}$'),
    PRIMARY KEY (subscription_version_id, item_no)
);

CREATE UNIQUE INDEX idx_subscription_items_one_plan
    ON subscription_items (subscription_version_id) WHERE item_role = 'PLAN';

CREATE TABLE subscription_item_quantities (
    subscription_version_id  TEXT        NOT NULL,
    item_no                  INT         NOT NULL,
    component_key            VARCHAR(64) NOT NULL,
    quantity                 NUMERIC     NOT NULL CHECK (quantity >= 0 AND scale(quantity) <= 4 AND quantity < 1000000000000000),
    PRIMARY KEY (subscription_version_id, item_no, component_key),
    FOREIGN KEY (subscription_version_id, item_no) REFERENCES subscription_items (subscription_version_id, item_no)
);

-- ── Renewal terms ─────────────────────────────────────────────────────────
CREATE TABLE subscription_terms (
    subscription_id          TEXT         NOT NULL REFERENCES subscriptions (subscription_id),
    term_no                  INT          NOT NULL CHECK (term_no >= 1),
    starts_at                TIMESTAMPTZ  NOT NULL,
    ends_at                  TIMESTAMPTZ  NOT NULL,
    price_version_id         TEXT         NOT NULL REFERENCES product_price_versions (price_version_id),
    auto_renew               BOOLEAN      NOT NULL,
    renewal_notice_days      INT          NOT NULL CHECK (renewal_notice_days >= 0),
    minimum_term_intervals   INT          NOT NULL CHECK (minimum_term_intervals >= 1),
    change_id                TEXT         NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL,
    created_by_principal_id  VARCHAR(255) NOT NULL,
    voided_at                TIMESTAMPTZ,
    voided_by_principal_id   VARCHAR(255),
    void_reason              TEXT,
    voided_by_change_id      TEXT,
    PRIMARY KEY (subscription_id, term_no),
    CONSTRAINT subscription_terms_positive CHECK (ends_at > starts_at),
    CONSTRAINT subscription_terms_void_all_or_nothing CHECK (
        (voided_at IS NULL) = (voided_by_principal_id IS NULL)
        AND (voided_at IS NULL) = (void_reason IS NULL)
        AND (voided_at IS NULL) = (voided_by_change_id IS NULL)),
    CONSTRAINT subscription_terms_void_before_start CHECK (voided_at IS NULL OR voided_at < starts_at),
    CONSTRAINT subscription_terms_no_overlap EXCLUDE USING gist (
        subscription_id WITH =, tstzrange(starts_at, ends_at, '[)') WITH &&) WHERE (voided_at IS NULL)
);

-- ── Append-only enforcement ───────────────────────────────────────────────

-- Versions and terms: the only permitted change is voiding, once, all four
-- void columns together. Anything else is refused.
CREATE FUNCTION enforce_void_only_update() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    void_cols TEXT[] := ARRAY['voided_at', 'voided_by_principal_id', 'void_reason', 'voided_by_change_id'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION '% rows are never deleted', TG_TABLE_NAME USING ERRCODE = 'CP001';
    END IF;
    IF OLD.voided_at IS NOT NULL THEN
        RAISE EXCEPTION 'a voided % row is final', TG_TABLE_NAME USING ERRCODE = 'CP001';
    END IF;
    IF (to_jsonb(NEW) - void_cols) IS DISTINCT FROM (to_jsonb(OLD) - void_cols) THEN
        RAISE EXCEPTION '% rows are append-only; the only permitted change is voiding a row that has not taken effect',
            TG_TABLE_NAME USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscription_versions_void_only
    BEFORE UPDATE OR DELETE ON subscription_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_void_only_update();

CREATE TRIGGER trg_subscription_terms_void_only
    BEFORE UPDATE OR DELETE ON subscription_terms
    FOR EACH ROW EXECUTE FUNCTION enforce_void_only_update();

CREATE FUNCTION reject_subscription_row_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable; a change is a new subscription version', TG_TABLE_NAME
        USING ERRCODE = 'CP001';
END;
$$;

CREATE TRIGGER trg_subscription_items_immutable
    BEFORE UPDATE OR DELETE ON subscription_items
    FOR EACH ROW EXECUTE FUNCTION reject_subscription_row_mutation();

CREATE TRIGGER trg_subscription_item_quantities_immutable
    BEFORE UPDATE OR DELETE ON subscription_item_quantities
    FOR EACH ROW EXECUTE FUNCTION reject_subscription_row_mutation();

-- ── Row-level security (tenant plane) ─────────────────────────────────────
--
-- subscriptions carries organization_id itself; every child resolves its
-- organization through the chain, the same subquery pattern as 000005.

ALTER TABLE subscriptions ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscriptions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscriptions FOR ALL
    USING (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

ALTER TABLE subscription_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscription_versions FOR ALL
    USING (subscription_id IN (SELECT subscription_id FROM subscriptions))
    WITH CHECK (subscription_id IN (SELECT subscription_id FROM subscriptions));

ALTER TABLE subscription_terms ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_terms FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscription_terms FOR ALL
    USING (subscription_id IN (SELECT subscription_id FROM subscriptions))
    WITH CHECK (subscription_id IN (SELECT subscription_id FROM subscriptions));

ALTER TABLE subscription_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_items FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscription_items FOR ALL
    USING (subscription_version_id IN (SELECT subscription_version_id FROM subscription_versions))
    WITH CHECK (subscription_version_id IN (SELECT subscription_version_id FROM subscription_versions));

ALTER TABLE subscription_item_quantities ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_item_quantities FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscription_item_quantities FOR ALL
    USING (subscription_version_id IN (SELECT subscription_version_id FROM subscription_versions))
    WITH CHECK (subscription_version_id IN (SELECT subscription_version_id FROM subscription_versions));
