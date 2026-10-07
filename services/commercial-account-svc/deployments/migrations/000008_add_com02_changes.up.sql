-- 000008_add_com02_changes.up.sql
-- COM-02 part 2b: plan transition rules, configuration changes, proration
-- evidence (ZS-SVC-Q-001 §4.2 plan transition rules; COM-CTRL-008, -009).

-- ── Transition rules (seller plane) ───────────────────────────────────────
--
-- Product policy as data. A change between two plans — or, from a plan to
-- itself, a quantity or add-on change — is allowed only by an active rule,
-- which fixes when it takes effect and how it is prorated. A rule is never
-- edited: it is retired and replaced, so the rule a past change ran under
-- stays exactly as it was.
CREATE TABLE plan_transition_rules (
    rule_id                  TEXT         PRIMARY KEY
        CHECK (rule_id ~ '^ctr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    from_product_id          TEXT         NOT NULL REFERENCES commercial_products (product_id),
    to_product_id            TEXT         NOT NULL REFERENCES commercial_products (product_id),
    timing                   VARCHAR(16)  NOT NULL CHECK (timing IN ('IMMEDIATE', 'NEXT_RENEWAL')),
    proration_method         VARCHAR(32)  NOT NULL CHECK (proration_method IN ('DAILY_HALF_EVEN', 'NONE')),
    created_at               TIMESTAMPTZ  NOT NULL,
    created_by_principal_id  VARCHAR(255) NOT NULL,
    retired_at               TIMESTAMPTZ,
    retired_by_principal_id  VARCHAR(255),
    retire_reason            TEXT,
    -- A change at the next renewal starts a fresh term at the new price:
    -- there is nothing to prorate.
    CONSTRAINT plan_transition_rules_renewal_not_prorated CHECK (timing = 'IMMEDIATE' OR proration_method = 'NONE'),
    CONSTRAINT plan_transition_rules_retire_evidence CHECK (
        (retired_at IS NULL) = (retired_by_principal_id IS NULL)
        AND (retired_at IS NULL) = (retire_reason IS NULL)
        AND (retire_reason IS NULL OR btrim(retire_reason) <> ''))
);

CREATE UNIQUE INDEX idx_plan_transition_rules_one_active
    ON plan_transition_rules (from_product_id, to_product_id) WHERE retired_at IS NULL;

CREATE FUNCTION enforce_transition_rule_retire_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    retire_cols TEXT[] := ARRAY['retired_at', 'retired_by_principal_id', 'retire_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'transition rule % cannot be deleted; retire it', OLD.rule_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.retired_at IS NOT NULL THEN
        RAISE EXCEPTION 'transition rule % is retired and final', OLD.rule_id USING ERRCODE = 'CP001';
    END IF;
    IF (to_jsonb(NEW) - retire_cols) IS DISTINCT FROM (to_jsonb(OLD) - retire_cols) THEN
        RAISE EXCEPTION 'transition rule % is never edited; retire it and create a new one', OLD.rule_id
            USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_plan_transition_rules_retire_only
    BEFORE UPDATE OR DELETE ON plan_transition_rules
    FOR EACH ROW EXECUTE FUNCTION enforce_transition_rule_retire_only();

ALTER TABLE plan_transition_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE plan_transition_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY transition_rules_read ON plan_transition_rules FOR SELECT USING (true);
CREATE POLICY transition_rules_seller_insert ON plan_transition_rules FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY transition_rules_seller_update ON plan_transition_rules FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- ── Version change types ──────────────────────────────────────────────────
ALTER TABLE subscription_versions DROP CONSTRAINT subscription_versions_change_type_check;
ALTER TABLE subscription_versions ADD CONSTRAINT subscription_versions_change_type_check
    CHECK (change_type IN ('STARTED', 'TRIAL_CONVERSION', 'TRIAL_EXPIRY', 'ACTIVATED',
                           'CANCELLATION_SCHEDULED', 'CANCELLATION_EFFECTIVE', 'CANCELED_NOW',
                           'REACTIVATED', 'TERM_EXPIRY', 'PLAN_CHANGED', 'QUANTITY_CHANGED', 'ADD_ON_CHANGED'));

-- ── Change evidence (tenant plane) ────────────────────────────────────────
--
-- One row per configuration change, sharing the change_id of the version it
-- wrote. It holds every input the proration used, so the amount can be
-- recomputed exactly from this row and the two bound price versions
-- (COM-CTRL-009). Rating results are kept unrounded (at most 8 places:
-- 4-place unit prices times 4-place quantities); proration amounts are in
-- the currency's minor units.
CREATE TABLE subscription_changes (
    change_id                TEXT         PRIMARY KEY
        CHECK (change_id ~ '^cchg_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    subscription_id          TEXT         NOT NULL REFERENCES subscriptions (subscription_id),
    change_kind              VARCHAR(16)  NOT NULL CHECK (change_kind IN ('PLAN_CHANGE', 'QUANTITY_CHANGE', 'ADD_ON_CHANGE')),
    rule_id                  TEXT         NOT NULL REFERENCES plan_transition_rules (rule_id),
    timing                   VARCHAR(16)  NOT NULL CHECK (timing IN ('IMMEDIATE', 'NEXT_RENEWAL')),
    proration_method         VARCHAR(32)  NOT NULL CHECK (proration_method IN ('DAILY_HALF_EVEN', 'NONE')),
    effective_at             TIMESTAMPTZ  NOT NULL,
    from_price_version_id    TEXT         NOT NULL REFERENCES product_price_versions (price_version_id),
    to_price_version_id      TEXT         NOT NULL REFERENCES product_price_versions (price_version_id),
    currency_code            CHAR(3)      NOT NULL REFERENCES commercial_currencies (currency_code),
    old_period_charge        NUMERIC      NOT NULL CHECK (old_period_charge >= 0 AND scale(old_period_charge) <= 8),
    new_period_charge        NUMERIC      NOT NULL CHECK (new_period_charge >= 0 AND scale(new_period_charge) <= 8),
    term_starts_at           TIMESTAMPTZ  NOT NULL,
    term_ends_at             TIMESTAMPTZ  NOT NULL,
    days_in_term             INT          CHECK (days_in_term > 0),
    days_remaining           INT          CHECK (days_remaining >= 0),
    proration_credit         NUMERIC      CHECK (proration_credit >= 0 AND scale(proration_credit) <= 4),
    proration_charge         NUMERIC      CHECK (proration_charge >= 0 AND scale(proration_charge) <= 4),
    proration_net            NUMERIC      CHECK (scale(proration_net) <= 4),
    quote_sha256             CHAR(64)     NOT NULL CHECK (quote_sha256 ~ '^[0-9a-f]{64}$'),
    channel                  VARCHAR(16)  NOT NULL CHECK (channel IN ('SELF_SERVICE', 'ASSISTED')),
    customer_basis_ref       VARCHAR(255),
    requested_by_principal_id VARCHAR(255) NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL,
    CONSTRAINT subscription_changes_proration_all_or_nothing CHECK (
        (proration_method = 'NONE') = (proration_net IS NULL)
        AND (proration_net IS NULL) = (proration_credit IS NULL)
        AND (proration_net IS NULL) = (proration_charge IS NULL)
        AND (proration_net IS NULL) = (days_remaining IS NULL)
        AND (proration_net IS NULL) = (days_in_term IS NULL)),
    CONSTRAINT subscription_changes_proration_only_now CHECK (timing = 'IMMEDIATE' OR proration_method = 'NONE'),
    CONSTRAINT subscription_changes_assisted_basis CHECK (
        channel <> 'ASSISTED' OR (customer_basis_ref IS NOT NULL AND btrim(customer_basis_ref) <> ''))
);

CREATE INDEX idx_subscription_changes_subscription ON subscription_changes (subscription_id, created_at);

CREATE TRIGGER trg_subscription_changes_immutable
    BEFORE UPDATE OR DELETE ON subscription_changes
    FOR EACH ROW EXECUTE FUNCTION reject_subscription_row_mutation();

ALTER TABLE subscription_changes ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_changes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscription_changes FOR ALL
    USING (subscription_id IN (SELECT subscription_id FROM subscriptions))
    WITH CHECK (subscription_id IN (SELECT subscription_id FROM subscriptions));
