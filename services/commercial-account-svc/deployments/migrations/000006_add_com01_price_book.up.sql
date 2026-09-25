-- 000006_add_com01_price_book.up.sql
-- COM-01 Product & Price Book (ZS-SVC-Q-001 §4.1, §7, controls COM-CTRL-001..006).
--
-- Additive: the doc7 Chunk 6 catalog (price_catalogs / plans /
-- entitlement_limits) is left untouched. commercial_subscriptions still has a
-- foreign key to plans, and re-binding subscriptions to ProductPriceVersion is
-- COM-02 work. The legacy tables are retired once nothing references them.
--
-- ── Seller plane, not tenant plane ─────────────────────────────────────────
--
-- The price book is ZoikoSuite's own catalogue: every tenant buys from the
-- same one, so there is no tenant column and no tenant predicate. It is still
-- ENABLE + FORCE ROW LEVEL SECURITY, with a policy keyed on a separate GUC,
-- app.commercial_plane:
--
--   reads   published/retired rows are visible to everyone; DRAFT / REVIEW /
--           APPROVED rows only to a transaction that declared
--           app.commercial_plane = 'seller'. Unpublished prices never reach a
--           tenant-scoped read, even through a handler bug.
--   writes  only a transaction that declared the seller plane may insert or
--           update. A tenant-scoped transaction (app.tenant_id = some
--           organization) cannot write the catalogue at all.
--   deletes no policy, so denied; the triggers below refuse them as well.
--
-- The GUC is deliberately NOT app.tenant_id: overloading the tenant setting
-- with a sentinel value would make "which organization is this" and "is this
-- the seller" the same question, and they are not (COM-CTRL-001).
--
-- ── Identifiers ────────────────────────────────────────────────────────────
--
-- Commercial IDs are prefixed (cprod_, cpv_, cpc_) rather than bare UUIDs.
-- Every tenant-plane service in this platform keys on bare UUIDs, so a bare
-- UUID arriving where a commercial ID is expected is, by construction, a
-- cross-plane identifier and is refused (§7 isolation invariant; negative
-- path #01). The CHECKs make that structural rather than a handler habit.
--
-- ── Money ──────────────────────────────────────────────────────────────────
--
-- Amounts are unconstrained NUMERIC with CHECK (scale(x) <= 4) rather than
-- NUMERIC(p,4): a scaled column silently rounds '0.00015' to 0.0002, and a
-- price book must refuse an amount it cannot store exactly, not reshape it
-- (negative path #46).
--
-- Custom SQLSTATE CP001 = protected price basis violation. The store maps it
-- to domain.ErrPriceVersionImmutable.

-- ── Currencies ─────────────────────────────────────────────────────────────
--
-- Minor units and sale eligibility are data, never a switch on currency code.
-- minor_units is fixed once written: changing it under existing prices would
-- reinterpret every amount already approved in that currency.
CREATE TABLE commercial_currencies (
    currency_code            CHAR(3)      PRIMARY KEY CHECK (currency_code ~ '^[A-Z]{3}$'),
    minor_units              SMALLINT     NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    sale_enabled             BOOLEAN      NOT NULL,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL,
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_by_principal_id  VARCHAR(255) NOT NULL
);

CREATE FUNCTION enforce_commercial_currency_immutability() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'commercial currency % cannot be deleted; disable it for sale instead', OLD.currency_code
            USING ERRCODE = 'CP001';
    END IF;
    IF NEW.currency_code <> OLD.currency_code
       OR NEW.minor_units <> OLD.minor_units
       OR NEW.created_at <> OLD.created_at
       OR NEW.created_by_principal_id <> OLD.created_by_principal_id THEN
        RAISE EXCEPTION 'commercial currency % minor units are fixed once written', OLD.currency_code
            USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_commercial_currencies_immutability
    BEFORE UPDATE OR DELETE ON commercial_currencies
    FOR EACH ROW EXECUTE FUNCTION enforce_commercial_currency_immutability();

-- ── Products ───────────────────────────────────────────────────────────────
--
-- Stable sellable identity only. The display label lives on the price version,
-- so a historical invoice renders the label the customer actually accepted
-- (negative path #47), not today's.
CREATE TABLE commercial_products (
    product_id               TEXT         PRIMARY KEY
        CHECK (product_id ~ '^cprod_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    product_code             VARCHAR(64)  NOT NULL UNIQUE CHECK (product_code ~ '^[a-z][a-z0-9_]{1,63}$'),
    product_kind             VARCHAR(16)  NOT NULL CHECK (product_kind IN ('PLAN', 'ADD_ON')),
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL
);

CREATE FUNCTION reject_commercial_product_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'commercial product % is immutable; its commercial content is versioned on product_price_versions',
        OLD.product_id USING ERRCODE = 'CP001';
END;
$$;

CREATE TRIGGER trg_commercial_products_immutable
    BEFORE UPDATE OR DELETE ON commercial_products
    FOR EACH ROW EXECUTE FUNCTION reject_commercial_product_mutation();

-- ── Price versions ─────────────────────────────────────────────────────────
CREATE TABLE product_price_versions (
    price_version_id               TEXT         PRIMARY KEY
        CHECK (price_version_id ~ '^cpv_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    product_id                     TEXT         NOT NULL REFERENCES commercial_products (product_id),
    version_number                 INT          NOT NULL CHECK (version_number >= 1),
    supersedes_price_version_id    TEXT         REFERENCES product_price_versions (price_version_id),

    display_name                   VARCHAR(255) NOT NULL CHECK (btrim(display_name) <> ''),
    billing_interval               VARCHAR(16)  NOT NULL CHECK (billing_interval IN ('MONTH', 'QUARTER', 'YEAR')),
    billing_interval_count         INT          NOT NULL DEFAULT 1 CHECK (billing_interval_count BETWEEN 1 AND 36),
    currency_code                  CHAR(3)      NOT NULL REFERENCES commercial_currencies (currency_code),
    -- Opaque market codes (data). array_to_string keeps the per-element format
    -- check inside a CHECK, which cannot run a subquery.
    market_codes                   TEXT[]       NOT NULL
        CHECK (cardinality(market_codes) >= 1
               AND array_to_string(market_codes, ',') ~ '^[A-Z]{2,8}(,[A-Z]{2,8})*$'),
    effective_from                 TIMESTAMPTZ  NOT NULL,
    effective_to                   TIMESTAMPTZ  CHECK (effective_to IS NULL OR effective_to > effective_from),
    change_reason                  TEXT         NOT NULL CHECK (btrim(change_reason) <> ''),

    -- CommercialTermVersion. NULL until SetCommercialTerms; required to submit.
    terms_document_ref             VARCHAR(512),
    terms_document_sha256          CHAR(64)     CHECK (terms_document_sha256 ~ '^[0-9a-f]{64}$'),
    auto_renew                     BOOLEAN,
    renewal_notice_days            INT          CHECK (renewal_notice_days >= 0),
    minimum_term_intervals         INT          CHECK (minimum_term_intervals >= 1),
    -- TrialPolicy (COM-CTRL-006). Absent means no trial exists; nothing assumes one.
    trial_duration_days            INT          CHECK (trial_duration_days BETWEEN 1 AND 365),
    trial_conversion               VARCHAR(32)
        CHECK (trial_conversion IN ('CONVERT_TO_PAID', 'CANCEL_AT_END', 'REQUIRE_CONFIRMATION')),
    trial_payment_method_required  BOOLEAN,
    terms_set_at                   TIMESTAMPTZ,
    terms_set_by_principal_id      VARCHAR(255),

    status                         VARCHAR(16)  NOT NULL
        CHECK (status IN ('DRAFT', 'REVIEW', 'APPROVED', 'PUBLISHED', 'RETIRED')),
    row_version                    INT          NOT NULL DEFAULT 1 CHECK (row_version >= 1),
    content_sha256                 CHAR(64)     CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),

    created_at                     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id        VARCHAR(255) NOT NULL,
    submitted_at                   TIMESTAMPTZ,
    submitted_by_principal_id      VARCHAR(255),
    approved_at                    TIMESTAMPTZ,
    approved_by_principal_id       VARCHAR(255),
    approved_content_sha256        CHAR(64),
    published_at                   TIMESTAMPTZ,
    published_by_principal_id      VARCHAR(255),
    retired_at                     TIMESTAMPTZ,
    retired_by_principal_id        VARCHAR(255),
    retire_reason                  TEXT,
    last_rejected_at               TIMESTAMPTZ,
    last_rejected_by_principal_id  VARCHAR(255),
    last_rejection_reason          TEXT,

    CONSTRAINT price_versions_product_version_unique UNIQUE (product_id, version_number),

    CONSTRAINT price_versions_terms_all_or_nothing CHECK (
        (terms_document_ref IS NULL AND terms_document_sha256 IS NULL AND auto_renew IS NULL
         AND renewal_notice_days IS NULL AND minimum_term_intervals IS NULL
         AND terms_set_at IS NULL AND terms_set_by_principal_id IS NULL)
        OR
        (terms_document_ref IS NOT NULL AND terms_document_sha256 IS NOT NULL AND auto_renew IS NOT NULL
         AND renewal_notice_days IS NOT NULL AND minimum_term_intervals IS NOT NULL
         AND terms_set_at IS NOT NULL AND terms_set_by_principal_id IS NOT NULL)
    ),
    CONSTRAINT price_versions_trial_all_or_nothing CHECK (
        (trial_duration_days IS NULL) = (trial_conversion IS NULL)
        AND (trial_duration_days IS NULL) = (trial_payment_method_required IS NULL)
    ),

    -- Lifecycle coherence: each state carries exactly the evidence it implies.
    CONSTRAINT price_versions_draft_is_clean CHECK (
        status <> 'DRAFT' OR (
            submitted_at IS NULL AND submitted_by_principal_id IS NULL AND content_sha256 IS NULL
            AND approved_at IS NULL AND approved_by_principal_id IS NULL AND approved_content_sha256 IS NULL
            AND published_at IS NULL AND published_by_principal_id IS NULL
            AND retired_at IS NULL AND retired_by_principal_id IS NULL AND retire_reason IS NULL)
    ),
    CONSTRAINT price_versions_submitted_evidence CHECK (
        status NOT IN ('REVIEW', 'APPROVED', 'PUBLISHED', 'RETIRED') OR (
            submitted_at IS NOT NULL AND submitted_by_principal_id IS NOT NULL
            AND content_sha256 IS NOT NULL AND terms_document_ref IS NOT NULL)
    ),
    CONSTRAINT price_versions_approved_evidence CHECK (
        status NOT IN ('APPROVED', 'PUBLISHED', 'RETIRED') OR (
            approved_at IS NOT NULL AND approved_by_principal_id IS NOT NULL
            AND approved_content_sha256 IS NOT NULL AND approved_content_sha256 = content_sha256)
    ),
    CONSTRAINT price_versions_published_evidence CHECK (
        status NOT IN ('PUBLISHED', 'RETIRED') OR (
            published_at IS NOT NULL AND published_by_principal_id IS NOT NULL
            AND published_at <= effective_from)
    ),
    CONSTRAINT price_versions_retired_evidence CHECK (
        status <> 'RETIRED' OR (
            retired_at IS NOT NULL AND retired_by_principal_id IS NOT NULL
            AND retire_reason IS NOT NULL AND btrim(retire_reason) <> '')
    ),
    CONSTRAINT price_versions_rejection_evidence CHECK (
        (last_rejected_at IS NULL) = (last_rejected_by_principal_id IS NULL)
        AND (last_rejected_at IS NULL) = (last_rejection_reason IS NULL)
    ),
    -- Maker-checker (COM-CTRL-004), restated here so a store bug cannot record
    -- a self-approval even if the application check were skipped.
    CONSTRAINT price_versions_approver_is_independent CHECK (
        approved_by_principal_id IS NULL OR (
            approved_by_principal_id <> created_by_principal_id
            AND approved_by_principal_id <> submitted_by_principal_id)
    )
);

-- One version in flight per product. Two concurrent drafts of the same
-- product would each believe they were "the next price".
CREATE UNIQUE INDEX idx_price_versions_one_in_flight
    ON product_price_versions (product_id)
    WHERE status IN ('DRAFT', 'REVIEW', 'APPROVED');

CREATE INDEX idx_price_versions_sellable
    ON product_price_versions (product_id, currency_code, effective_from DESC, version_number DESC)
    WHERE status = 'PUBLISHED';

-- Lifecycle trigger. Each transition names the exact columns it may change;
-- everything else is compared as a whole row, so a column added later is
-- protected by default instead of by someone remembering to list it.
CREATE FUNCTION enforce_price_version_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    allowed TEXT[];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'price version % cannot be deleted; retire it instead', OLD.price_version_id
            USING ERRCODE = 'CP001';
    END IF;

    IF OLD.status = 'RETIRED' THEN
        RAISE EXCEPTION 'retired price version % is immutable', OLD.price_version_id
            USING ERRCODE = 'CP001';
    END IF;

    allowed := CASE
        WHEN OLD.status = 'DRAFT' AND NEW.status = 'DRAFT' THEN ARRAY[
            'row_version', 'display_name', 'billing_interval', 'billing_interval_count', 'currency_code',
            'market_codes', 'effective_from', 'effective_to', 'change_reason',
            'terms_document_ref', 'terms_document_sha256', 'auto_renew', 'renewal_notice_days',
            'minimum_term_intervals', 'trial_duration_days', 'trial_conversion',
            'trial_payment_method_required', 'terms_set_at', 'terms_set_by_principal_id']
        WHEN OLD.status = 'DRAFT' AND NEW.status = 'REVIEW' THEN ARRAY[
            'status', 'row_version', 'submitted_at', 'submitted_by_principal_id', 'content_sha256']
        WHEN OLD.status = 'REVIEW' AND NEW.status = 'APPROVED' THEN ARRAY[
            'status', 'row_version', 'approved_at', 'approved_by_principal_id', 'approved_content_sha256']
        WHEN OLD.status = 'REVIEW' AND NEW.status = 'DRAFT' THEN ARRAY[
            'status', 'row_version', 'submitted_at', 'submitted_by_principal_id', 'content_sha256',
            'last_rejected_at', 'last_rejected_by_principal_id', 'last_rejection_reason']
        WHEN OLD.status = 'APPROVED' AND NEW.status = 'PUBLISHED' THEN ARRAY[
            'status', 'row_version', 'published_at', 'published_by_principal_id']
        WHEN OLD.status = 'PUBLISHED' AND NEW.status = 'RETIRED' THEN ARRAY[
            'status', 'row_version', 'retired_at', 'retired_by_principal_id', 'retire_reason']
        ELSE NULL
    END;

    IF allowed IS NULL THEN
        RAISE EXCEPTION 'price version % cannot move from % to %; published prices are never edited in place',
            OLD.price_version_id, OLD.status, NEW.status USING ERRCODE = 'CP001';
    END IF;

    IF (to_jsonb(NEW) - allowed) IS DISTINCT FROM (to_jsonb(OLD) - allowed) THEN
        RAISE EXCEPTION 'price version % (%): protected columns cannot change on a % -> % transition',
            OLD.price_version_id, OLD.status, OLD.status, NEW.status USING ERRCODE = 'CP001';
    END IF;

    -- Optimistic concurrency, enforced at the source: every accepted change
    -- advances row_version by exactly one, so an ETag can never be reused.
    IF NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'price version % row_version must advance by one (was %, got %)',
            OLD.price_version_id, OLD.row_version, NEW.row_version USING ERRCODE = 'CP001';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_price_versions_lifecycle
    BEFORE UPDATE OR DELETE ON product_price_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_price_version_lifecycle();

-- ── Price components ───────────────────────────────────────────────────────
CREATE TABLE price_components (
    price_component_id       TEXT         PRIMARY KEY
        CHECK (price_component_id ~ '^cpc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    price_version_id         TEXT         NOT NULL REFERENCES product_price_versions (price_version_id),
    component_key            VARCHAR(64)  NOT NULL CHECK (component_key ~ '^[a-z][a-z0-9_]{0,63}$'),
    component_type           VARCHAR(16)  NOT NULL
        CHECK (component_type IN ('RECURRING_FIXED', 'PER_UNIT', 'METERED', 'ONE_TIME', 'DISCOUNT')),

    amount                   NUMERIC      CHECK (amount >= 0 AND scale(amount) <= 4 AND amount < 1000000000000000),
    billing_timing           VARCHAR(16)  CHECK (billing_timing IN ('IN_ADVANCE', 'IN_ARREARS')),

    unit_name                VARCHAR(64),
    included_quantity        NUMERIC      CHECK (included_quantity >= 0 AND scale(included_quantity) <= 4),
    minimum_quantity         NUMERIC      CHECK (minimum_quantity >= 0 AND scale(minimum_quantity) <= 4),
    maximum_quantity         NUMERIC      CHECK (maximum_quantity > 0 AND scale(maximum_quantity) <= 4),
    quantity_rounding        VARCHAR(8)   CHECK (quantity_rounding IN ('NONE', 'UP', 'DOWN')),
    tier_mode                VARCHAR(16)  CHECK (tier_mode IN ('VOLUME', 'GRADUATED')),

    -- COM-04 meter reference. Opaque until the meter registry exists; a version
    -- carrying a METERED component cannot be submitted before then.
    meter_key                VARCHAR(128),
    meter_version            INT          CHECK (meter_version >= 1),
    aggregation_method       VARCHAR(16)  CHECK (aggregation_method IN ('SUM', 'MAX', 'LAST', 'UNIQUE_COUNT')),

    trigger_event            VARCHAR(64),
    eligibility_code         VARCHAR(64),
    expires_after_days       INT          CHECK (expires_after_days >= 1),

    discount_type            VARCHAR(16)  CHECK (discount_type IN ('PERCENT', 'FIXED_AMOUNT')),
    discount_value           NUMERIC      CHECK (discount_value > 0 AND scale(discount_value) <= 4),
    discount_cap_amount      NUMERIC      CHECK (discount_cap_amount > 0 AND scale(discount_cap_amount) <= 4),
    duration_intervals       INT          CHECK (duration_intervals >= 1),
    requires_approval        BOOLEAN,

    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by_principal_id  VARCHAR(255) NOT NULL,

    CONSTRAINT price_components_key_unique UNIQUE (price_version_id, component_key),
    -- Target of the tier table's composite FK, so a tier can never point at a
    -- component of a different version than the one it claims.
    CONSTRAINT price_components_id_version_unique UNIQUE (price_component_id, price_version_id),

    CONSTRAINT price_components_quantity_bounds CHECK (
        minimum_quantity IS NULL OR maximum_quantity IS NULL OR maximum_quantity >= minimum_quantity
    ),

    -- Per-type shape: each type carries exactly the attributes the price
    -- component model (§4.1) requires of it, and none of the others.
    CONSTRAINT price_components_shape CHECK (
        CASE component_type
        WHEN 'RECURRING_FIXED' THEN
            amount IS NOT NULL AND billing_timing IS NOT NULL
            AND unit_name IS NULL AND included_quantity IS NULL AND minimum_quantity IS NULL
            AND maximum_quantity IS NULL AND quantity_rounding IS NULL AND tier_mode IS NULL
            AND meter_key IS NULL AND meter_version IS NULL AND aggregation_method IS NULL
            AND trigger_event IS NULL AND eligibility_code IS NULL AND expires_after_days IS NULL
            AND discount_type IS NULL AND discount_value IS NULL AND discount_cap_amount IS NULL
            AND duration_intervals IS NULL AND requires_approval IS NULL
        WHEN 'PER_UNIT' THEN
            unit_name IS NOT NULL AND billing_timing IS NOT NULL AND included_quantity IS NOT NULL
            AND quantity_rounding IS NOT NULL AND ((amount IS NULL) <> (tier_mode IS NULL))
            AND meter_key IS NULL AND meter_version IS NULL AND aggregation_method IS NULL
            AND trigger_event IS NULL AND eligibility_code IS NULL AND expires_after_days IS NULL
            AND discount_type IS NULL AND discount_value IS NULL AND discount_cap_amount IS NULL
            AND duration_intervals IS NULL AND requires_approval IS NULL
        WHEN 'METERED' THEN
            meter_key IS NOT NULL AND meter_version IS NOT NULL AND aggregation_method IS NOT NULL
            AND included_quantity IS NOT NULL AND billing_timing = 'IN_ARREARS'
            AND ((amount IS NULL) <> (tier_mode IS NULL))
            AND unit_name IS NULL AND minimum_quantity IS NULL AND maximum_quantity IS NULL
            AND quantity_rounding IS NULL
            AND trigger_event IS NULL AND eligibility_code IS NULL AND expires_after_days IS NULL
            AND discount_type IS NULL AND discount_value IS NULL AND discount_cap_amount IS NULL
            AND duration_intervals IS NULL AND requires_approval IS NULL
        WHEN 'ONE_TIME' THEN
            amount IS NOT NULL AND trigger_event IS NOT NULL
            AND billing_timing IS NULL AND unit_name IS NULL AND included_quantity IS NULL
            AND minimum_quantity IS NULL AND maximum_quantity IS NULL AND quantity_rounding IS NULL
            AND tier_mode IS NULL AND meter_key IS NULL AND meter_version IS NULL
            AND aggregation_method IS NULL
            AND discount_type IS NULL AND discount_value IS NULL AND discount_cap_amount IS NULL
            AND duration_intervals IS NULL AND requires_approval IS NULL
        WHEN 'DISCOUNT' THEN
            discount_type IS NOT NULL AND discount_value IS NOT NULL AND duration_intervals IS NOT NULL
            AND eligibility_code IS NOT NULL AND requires_approval IS NOT NULL
            AND (discount_type <> 'PERCENT' OR discount_value <= 100)
            AND amount IS NULL AND billing_timing IS NULL AND unit_name IS NULL
            AND included_quantity IS NULL AND minimum_quantity IS NULL AND maximum_quantity IS NULL
            AND quantity_rounding IS NULL AND tier_mode IS NULL AND meter_key IS NULL
            AND meter_version IS NULL AND aggregation_method IS NULL AND trigger_event IS NULL
            AND expires_after_days IS NULL
        END
    )
);

CREATE INDEX idx_price_components_version ON price_components (price_version_id);

-- Tier / overage bands. price_version_id is carried (and pinned by the
-- composite FK) so the draft-only trigger and the RLS policy can check the
-- parent version without a join through the component.
CREATE TABLE price_component_tiers (
    price_component_id  TEXT     NOT NULL,
    price_version_id    TEXT     NOT NULL,
    tier_index          INT      NOT NULL CHECK (tier_index >= 1),
    up_to_quantity      NUMERIC  CHECK (up_to_quantity > 0 AND scale(up_to_quantity) <= 4),
    unit_amount         NUMERIC  NOT NULL CHECK (unit_amount >= 0 AND scale(unit_amount) <= 4 AND unit_amount < 1000000000000000),
    flat_amount         NUMERIC  CHECK (flat_amount >= 0 AND scale(flat_amount) <= 4 AND flat_amount < 1000000000000000),
    PRIMARY KEY (price_component_id, tier_index),
    FOREIGN KEY (price_component_id, price_version_id)
        REFERENCES price_components (price_component_id, price_version_id)
);

-- Plan capability matrix (GetPlanCapabilities). NULL limit_value = unlimited.
-- COM-03 consumes this; it does not decide access by itself.
CREATE TABLE price_version_capabilities (
    price_version_id  TEXT          NOT NULL REFERENCES product_price_versions (price_version_id),
    capability_key    VARCHAR(128)  NOT NULL CHECK (capability_key ~ '^[a-z][a-z0-9_.:-]{1,127}$'),
    limit_value       BIGINT        CHECK (limit_value >= 0),
    limit_unit        VARCHAR(32),
    PRIMARY KEY (price_version_id, capability_key),
    CHECK ((limit_value IS NULL) OR (limit_unit IS NOT NULL))
);

-- Children of a version change only while it is DRAFT, and never in place.
-- FOR SHARE takes a row lock on the parent: a concurrent SubmitForApproval
-- (which locks the version FOR UPDATE before hashing it) either finishes
-- first — and this insert then sees REVIEW and is refused — or waits for
-- this insert, and its hash includes the new row. Either way the approved
-- hash covers exactly what was approved.
CREATE FUNCTION enforce_price_child_draft_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    v_id     TEXT;
    v_status TEXT;
BEGIN
    IF TG_OP = 'UPDATE' THEN
        RAISE EXCEPTION '% rows are never updated in place; replace them on a draft version', TG_TABLE_NAME
            USING ERRCODE = 'CP001';
    END IF;

    v_id := CASE WHEN TG_OP = 'DELETE' THEN OLD.price_version_id ELSE NEW.price_version_id END;

    SELECT status INTO v_status
      FROM product_price_versions
     WHERE price_version_id = v_id
       FOR SHARE;

    IF v_status IS DISTINCT FROM 'DRAFT' THEN
        RAISE EXCEPTION '% of price version % cannot change: version is % (only DRAFT content is editable)',
            TG_TABLE_NAME, v_id, COALESCE(v_status, 'missing') USING ERRCODE = 'CP001';
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_price_components_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON price_components
    FOR EACH ROW EXECUTE FUNCTION enforce_price_child_draft_only();

CREATE TRIGGER trg_price_component_tiers_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON price_component_tiers
    FOR EACH ROW EXECUTE FUNCTION enforce_price_child_draft_only();

CREATE TRIGGER trg_price_version_capabilities_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON price_version_capabilities
    FOR EACH ROW EXECUTE FUNCTION enforce_price_child_draft_only();

-- ── Idempotency ledger ─────────────────────────────────────────────────────
--
-- Written in the SAME transaction as the business change it guards, before
-- that change. A concurrent request with the same key blocks on the unique
-- index until the first commits, then sees the claim and replays; if the
-- first rolls back, the second proceeds. No window exists in which both run.
--
-- owner_scope is 'seller' for price-book commands, or the organization id
-- for tenant-plane commands in later waves; keys are additionally scoped per
-- principal so two operators can never collide on the same key.
CREATE TABLE commercial_idempotency_keys (
    owner_scope      TEXT          NOT NULL CHECK (btrim(owner_scope) <> ''),
    principal_id     VARCHAR(255)  NOT NULL,
    idempotency_key  VARCHAR(255)  NOT NULL CHECK (btrim(idempotency_key) <> ''),
    operation        VARCHAR(64)   NOT NULL,
    request_sha256   CHAR(64)      NOT NULL CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
    resource_id      TEXT          NOT NULL,
    created_at       TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    PRIMARY KEY (owner_scope, principal_id, idempotency_key)
);

CREATE FUNCTION reject_idempotency_key_update() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'idempotency claims are never rewritten' USING ERRCODE = 'CP001';
END;
$$;

CREATE TRIGGER trg_idempotency_keys_no_update
    BEFORE UPDATE ON commercial_idempotency_keys
    FOR EACH ROW EXECUTE FUNCTION reject_idempotency_key_update();

-- ── Row-level security ─────────────────────────────────────────────────────

ALTER TABLE commercial_currencies ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_currencies FORCE ROW LEVEL SECURITY;
CREATE POLICY currencies_read ON commercial_currencies FOR SELECT USING (true);
CREATE POLICY currencies_seller_insert ON commercial_currencies FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY currencies_seller_update ON commercial_currencies FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

ALTER TABLE product_price_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE product_price_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY price_versions_read ON product_price_versions FOR SELECT
    USING (status IN ('PUBLISHED', 'RETIRED') OR current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY price_versions_seller_insert ON product_price_versions FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY price_versions_seller_update ON product_price_versions FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- A product with no published version is an unannounced product: its code is
-- visible only to the seller plane. The subquery is itself filtered by
-- price_versions_read, so outside the seller plane it only sees published or
-- retired versions.
ALTER TABLE commercial_products ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_products FORCE ROW LEVEL SECURITY;
CREATE POLICY products_read ON commercial_products FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR product_id IN (SELECT product_id FROM product_price_versions));
CREATE POLICY products_seller_insert ON commercial_products FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
-- SELECT ... FOR UPDATE is subject to UPDATE policies, and CreateDraftVersion
-- row-locks the product to serialize version numbering. This policy exists
-- only to permit that lock; any actual UPDATE is still refused by
-- trg_commercial_products_immutable.
CREATE POLICY products_seller_lock ON commercial_products FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

ALTER TABLE price_components ENABLE ROW LEVEL SECURITY;
ALTER TABLE price_components FORCE ROW LEVEL SECURITY;
CREATE POLICY price_components_read ON price_components FOR SELECT
    USING (price_version_id IN (SELECT price_version_id FROM product_price_versions));
CREATE POLICY price_components_seller_insert ON price_components FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY price_components_seller_delete ON price_components FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

ALTER TABLE price_component_tiers ENABLE ROW LEVEL SECURITY;
ALTER TABLE price_component_tiers FORCE ROW LEVEL SECURITY;
CREATE POLICY price_component_tiers_read ON price_component_tiers FOR SELECT
    USING (price_version_id IN (SELECT price_version_id FROM product_price_versions));
CREATE POLICY price_component_tiers_seller_insert ON price_component_tiers FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY price_component_tiers_seller_delete ON price_component_tiers FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

ALTER TABLE price_version_capabilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE price_version_capabilities FORCE ROW LEVEL SECURITY;
CREATE POLICY price_capabilities_read ON price_version_capabilities FOR SELECT
    USING (price_version_id IN (SELECT price_version_id FROM product_price_versions));
CREATE POLICY price_capabilities_seller_insert ON price_version_capabilities FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY price_capabilities_seller_delete ON price_version_capabilities FOR DELETE
    USING (current_setting('app.commercial_plane', true) = 'seller');

ALTER TABLE commercial_idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_owner ON commercial_idempotency_keys FOR ALL
    USING (owner_scope = CASE
                WHEN current_setting('app.commercial_plane', true) = 'seller' THEN 'seller'
                ELSE NULLIF(current_setting('app.tenant_id', true), '')
           END)
    WITH CHECK (owner_scope = CASE
                WHEN current_setting('app.commercial_plane', true) = 'seller' THEN 'seller'
                ELSE NULLIF(current_setting('app.tenant_id', true), '')
           END);
