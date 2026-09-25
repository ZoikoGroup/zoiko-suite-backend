-- 000009_add_com02_boundaries_discounts_migrations.up.sql
-- COM-02 part 2c (ZS-SVC-Q-001 §4.1, §4.2; COM-CTRL-003, -005; negative
-- paths #06, #38):
--   1. the boundary queue behind renewals and effective-time events,
--   2. discount applications under delegated authority,
--   3. price migration offers.

-- ── 1. Boundary queue ─────────────────────────────────────────────────────
--
-- Subscription state is already correct at every instant (future-dated
-- versions); what needs a process is (a) renewing an auto-renewing term when
-- it ends and (b) publishing the event for a version at the moment it takes
-- effect. Rows are written by triggers, so no code path that writes a
-- version or a term can forget to schedule its boundary.
--
-- RLS: a row is visible to its own organization (so the tenant-scoped
-- trigger can write it) and to a transaction that declared
-- app.commercial_plane = 'boundary_worker' (so the worker can claim across
-- organizations). It carries identifiers and times only.
CREATE TABLE subscription_boundary_queue (
    boundary_id              UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id          UUID         NOT NULL,
    subscription_id          TEXT         NOT NULL REFERENCES subscriptions (subscription_id),
    kind                     VARCHAR(20)  NOT NULL CHECK (kind IN ('VERSION_EFFECTIVE', 'TERM_END')),
    subscription_version_id  TEXT         REFERENCES subscription_versions (subscription_version_id),
    term_no                  INT,
    due_at                   TIMESTAMPTZ  NOT NULL,
    next_attempt_at          TIMESTAMPTZ  NOT NULL,
    attempts                 INT          NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error               TEXT,
    status                   VARCHAR(16)  NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING', 'DONE', 'FAILED')),
    outcome                  VARCHAR(32),
    processed_at             TIMESTAMPTZ,
    CONSTRAINT boundary_kind_target CHECK (
        (kind = 'VERSION_EFFECTIVE' AND subscription_version_id IS NOT NULL AND term_no IS NULL)
        OR (kind = 'TERM_END' AND term_no IS NOT NULL AND subscription_version_id IS NULL)),
    CONSTRAINT boundary_done_evidence CHECK ((status = 'PENDING') = (processed_at IS NULL)),
    CONSTRAINT boundary_one_per_target UNIQUE NULLS NOT DISTINCT (subscription_id, kind, subscription_version_id, term_no)
);

CREATE INDEX idx_boundary_queue_due ON subscription_boundary_queue (next_attempt_at) WHERE status = 'PENDING';

CREATE FUNCTION enqueue_version_boundary() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Only a version that takes effect later needs a boundary event; one
    -- effective at once had its event published by the command itself.
    IF NEW.effective_from > NEW.created_at THEN
        INSERT INTO subscription_boundary_queue
            (organization_id, subscription_id, kind, subscription_version_id, due_at, next_attempt_at)
        SELECT s.organization_id, NEW.subscription_id, 'VERSION_EFFECTIVE', NEW.subscription_version_id,
               NEW.effective_from, NEW.effective_from
          FROM subscriptions s WHERE s.subscription_id = NEW.subscription_id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscription_versions_enqueue
    AFTER INSERT ON subscription_versions
    FOR EACH ROW EXECUTE FUNCTION enqueue_version_boundary();

CREATE FUNCTION enqueue_term_boundary() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    -- Every term end is queued; the worker decides at the boundary whether
    -- to renew (auto-renewing, still in force) or to skip.
    INSERT INTO subscription_boundary_queue
        (organization_id, subscription_id, kind, term_no, due_at, next_attempt_at)
    SELECT s.organization_id, NEW.subscription_id, 'TERM_END', NEW.term_no, NEW.ends_at, NEW.ends_at
      FROM subscriptions s WHERE s.subscription_id = NEW.subscription_id;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscription_terms_enqueue
    AFTER INSERT ON subscription_terms
    FOR EACH ROW EXECUTE FUNCTION enqueue_term_boundary();

ALTER TABLE subscription_boundary_queue ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_boundary_queue FORCE ROW LEVEL SECURITY;
CREATE POLICY boundary_queue_access ON subscription_boundary_queue FOR ALL
    USING (current_setting('app.commercial_plane', true) = 'boundary_worker'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (current_setting('app.commercial_plane', true) = 'boundary_worker'
                OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- Backfill boundaries for subscriptions written before this migration. The
-- migration role must be able to read them (a superuser, or the table owner
-- without FORCE applying to it); on a fresh database this inserts nothing.
INSERT INTO subscription_boundary_queue (organization_id, subscription_id, kind, subscription_version_id, due_at, next_attempt_at)
SELECT s.organization_id, v.subscription_id, 'VERSION_EFFECTIVE', v.subscription_version_id, v.effective_from, v.effective_from
  FROM subscription_versions v JOIN subscriptions s USING (subscription_id)
 WHERE v.voided_at IS NULL AND v.effective_from > now();
INSERT INTO subscription_boundary_queue (organization_id, subscription_id, kind, term_no, due_at, next_attempt_at)
SELECT s.organization_id, t.subscription_id, 'TERM_END', t.term_no, t.ends_at, t.ends_at
  FROM subscription_terms t JOIN subscriptions s USING (subscription_id)
 WHERE t.voided_at IS NULL AND t.ends_at > now();

-- ── 2. Discount applications ──────────────────────────────────────────────
--
-- A discount is one of the catalogue's approved DISCOUNT components, applied
-- to one subscription through a governed command — never an edit to an
-- invoice line (§4.1 price component model). A component the catalogue marks
-- requires_approval needs a second person (COM-CTRL-005; negative path #06);
-- one it does not is approved on catalogue policy, and says so.
CREATE TABLE subscription_discounts (
    discount_application_id  TEXT         PRIMARY KEY
        CHECK (discount_application_id ~ '^cdsc_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    subscription_id          TEXT         NOT NULL REFERENCES subscriptions (subscription_id),
    price_version_id         TEXT         NOT NULL REFERENCES product_price_versions (price_version_id),
    component_key            VARCHAR(64)  NOT NULL,
    status                   VARCHAR(16)  NOT NULL CHECK (status IN ('PROPOSED', 'APPROVED', 'REJECTED', 'WITHDRAWN')),
    reason                   TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    customer_basis_ref       VARCHAR(255) NOT NULL CHECK (btrim(customer_basis_ref) <> ''),
    requested_by_principal_id VARCHAR(255) NOT NULL,
    requested_at             TIMESTAMPTZ  NOT NULL,
    approval_basis           VARCHAR(16)  CHECK (approval_basis IN ('APPROVER', 'CATALOG_POLICY')),
    approved_by_principal_id VARCHAR(255),
    approved_at              TIMESTAMPTZ,
    decided_reason           TEXT,
    decided_by_principal_id  VARCHAR(255),
    decided_at               TIMESTAMPTZ,
    row_version              INT          NOT NULL DEFAULT 1 CHECK (row_version >= 1),
    CONSTRAINT discounts_approval_evidence CHECK (
        status <> 'APPROVED' OR (approval_basis IS NOT NULL AND approved_at IS NOT NULL
            AND (approval_basis = 'CATALOG_POLICY' OR approved_by_principal_id IS NOT NULL))),
    CONSTRAINT discounts_decision_evidence CHECK (
        status NOT IN ('REJECTED', 'WITHDRAWN') OR (decided_by_principal_id IS NOT NULL AND decided_at IS NOT NULL
            AND decided_reason IS NOT NULL AND btrim(decided_reason) <> '')),
    -- Nobody approves their own discount.
    CONSTRAINT discounts_approver_is_independent CHECK (
        approved_by_principal_id IS NULL OR approved_by_principal_id <> requested_by_principal_id)
);

CREATE UNIQUE INDEX idx_subscription_discounts_one_live
    ON subscription_discounts (subscription_id, price_version_id, component_key)
    WHERE status IN ('PROPOSED', 'APPROVED');

CREATE FUNCTION enforce_discount_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    allowed TEXT[];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'discount application % cannot be deleted', OLD.discount_application_id USING ERRCODE = 'CP001';
    END IF;
    allowed := CASE
        WHEN OLD.status = 'PROPOSED' AND NEW.status = 'APPROVED' THEN
            ARRAY['status', 'row_version', 'approval_basis', 'approved_by_principal_id', 'approved_at']
        WHEN OLD.status = 'PROPOSED' AND NEW.status IN ('REJECTED', 'WITHDRAWN') THEN
            ARRAY['status', 'row_version', 'decided_reason', 'decided_by_principal_id', 'decided_at']
        ELSE NULL
    END;
    IF allowed IS NULL THEN
        RAISE EXCEPTION 'discount application % cannot move from % to %', OLD.discount_application_id, OLD.status, NEW.status
            USING ERRCODE = 'CP001';
    END IF;
    IF (to_jsonb(NEW) - allowed) IS DISTINCT FROM (to_jsonb(OLD) - allowed) THEN
        RAISE EXCEPTION 'discount application %: protected fields cannot change', OLD.discount_application_id
            USING ERRCODE = 'CP001';
    END IF;
    IF NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'discount application % row_version must advance by one', OLD.discount_application_id
            USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_subscription_discounts_lifecycle
    BEFORE UPDATE OR DELETE ON subscription_discounts
    FOR EACH ROW EXECUTE FUNCTION enforce_discount_lifecycle();

ALTER TABLE subscription_discounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_discounts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON subscription_discounts FOR ALL
    USING (subscription_id IN (SELECT subscription_id FROM subscriptions))
    WITH CHECK (subscription_id IN (SELECT subscription_id FROM subscriptions));

-- ── 3. Price migration offers ─────────────────────────────────────────────
--
-- Grandfathering is explicit (§4.2): a newer price never reprices anyone.
-- Moving existing subscribers to it is a governed offer — from one price
-- version to a later one of the same product — that must name who is
-- eligible before it can be published (negative path #38), is published by
-- someone other than its creator, and is taken up by each customer at their
-- next renewal.
CREATE TABLE price_migration_offers (
    migration_offer_id       TEXT         PRIMARY KEY
        CHECK (migration_offer_id ~ '^cmig_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    product_id               TEXT         NOT NULL REFERENCES commercial_products (product_id),
    from_price_version_id    TEXT         NOT NULL REFERENCES product_price_versions (price_version_id),
    to_price_version_id      TEXT         NOT NULL REFERENCES product_price_versions (price_version_id),
    eligibility_mode         VARCHAR(24)  CHECK (eligibility_mode IN ('ALL_ON_FROM_VERSION', 'LISTED_SUBSCRIPTIONS')),
    accept_by                TIMESTAMPTZ,
    reason                   TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    status                   VARCHAR(16)  NOT NULL CHECK (status IN ('DRAFT', 'PUBLISHED', 'WITHDRAWN')),
    row_version              INT          NOT NULL DEFAULT 1 CHECK (row_version >= 1),
    created_at               TIMESTAMPTZ  NOT NULL,
    created_by_principal_id  VARCHAR(255) NOT NULL,
    published_at             TIMESTAMPTZ,
    published_by_principal_id VARCHAR(255),
    withdrawn_at             TIMESTAMPTZ,
    withdrawn_by_principal_id VARCHAR(255),
    withdraw_reason          TEXT,
    CONSTRAINT migration_offer_distinct_versions CHECK (from_price_version_id <> to_price_version_id),
    CONSTRAINT migration_offer_published_evidence CHECK (
        status = 'DRAFT' OR (published_at IS NOT NULL AND published_by_principal_id IS NOT NULL
                             AND eligibility_mode IS NOT NULL) OR status = 'WITHDRAWN'),
    CONSTRAINT migration_offer_publisher_is_independent CHECK (
        published_by_principal_id IS NULL OR published_by_principal_id <> created_by_principal_id),
    CONSTRAINT migration_offer_withdrawn_evidence CHECK (
        status <> 'WITHDRAWN' OR (withdrawn_at IS NOT NULL AND withdrawn_by_principal_id IS NOT NULL
                                  AND withdraw_reason IS NOT NULL AND btrim(withdraw_reason) <> ''))
);

CREATE TABLE price_migration_offer_targets (
    migration_offer_id       TEXT  NOT NULL REFERENCES price_migration_offers (migration_offer_id),
    subscription_id          TEXT  NOT NULL REFERENCES subscriptions (subscription_id),
    PRIMARY KEY (migration_offer_id, subscription_id)
);

CREATE FUNCTION enforce_migration_offer_lifecycle() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    allowed TEXT[];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'migration offer % cannot be deleted; withdraw it', OLD.migration_offer_id USING ERRCODE = 'CP001';
    END IF;
    allowed := CASE
        WHEN OLD.status = 'DRAFT' AND NEW.status = 'PUBLISHED' THEN
            ARRAY['status', 'row_version', 'published_at', 'published_by_principal_id']
        WHEN OLD.status IN ('DRAFT', 'PUBLISHED') AND NEW.status = 'WITHDRAWN' THEN
            ARRAY['status', 'row_version', 'withdrawn_at', 'withdrawn_by_principal_id', 'withdraw_reason']
        ELSE NULL
    END;
    IF allowed IS NULL THEN
        RAISE EXCEPTION 'migration offer % cannot move from % to %', OLD.migration_offer_id, OLD.status, NEW.status
            USING ERRCODE = 'CP001';
    END IF;
    IF (to_jsonb(NEW) - allowed) IS DISTINCT FROM (to_jsonb(OLD) - allowed) THEN
        RAISE EXCEPTION 'migration offer %: protected fields cannot change', OLD.migration_offer_id USING ERRCODE = 'CP001';
    END IF;
    IF NEW.row_version <> OLD.row_version + 1 THEN
        RAISE EXCEPTION 'migration offer % row_version must advance by one', OLD.migration_offer_id USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_price_migration_offers_lifecycle
    BEFORE UPDATE OR DELETE ON price_migration_offers
    FOR EACH ROW EXECUTE FUNCTION enforce_migration_offer_lifecycle();

-- Targets are fixed while the offer is a draft and frozen once it is not.
CREATE FUNCTION enforce_migration_target_draft_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    o_status TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'migration offer targets are never changed' USING ERRCODE = 'CP001';
    END IF;
    SELECT status INTO o_status FROM price_migration_offers WHERE migration_offer_id = NEW.migration_offer_id FOR SHARE;
    IF o_status IS DISTINCT FROM 'DRAFT' THEN
        RAISE EXCEPTION 'targets can only be added to a DRAFT migration offer' USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_price_migration_offer_targets_draft_only
    BEFORE INSERT OR UPDATE OR DELETE ON price_migration_offer_targets
    FOR EACH ROW EXECUTE FUNCTION enforce_migration_target_draft_only();

-- Published offers are readable by customers (to see and accept them);
-- drafts only by the seller plane. A customer sees a target row only for
-- its own subscriptions, through the subscriptions policy.
ALTER TABLE price_migration_offers ENABLE ROW LEVEL SECURITY;
ALTER TABLE price_migration_offers FORCE ROW LEVEL SECURITY;
CREATE POLICY migration_offers_read ON price_migration_offers FOR SELECT
    USING (status = 'PUBLISHED' OR current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY migration_offers_seller_insert ON price_migration_offers FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY migration_offers_seller_update ON price_migration_offers FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

ALTER TABLE price_migration_offer_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE price_migration_offer_targets FORCE ROW LEVEL SECURITY;
CREATE POLICY migration_targets_read ON price_migration_offer_targets FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR subscription_id IN (SELECT subscription_id FROM subscriptions));
CREATE POLICY migration_targets_seller_insert ON price_migration_offer_targets FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- ── Changes: a migration is governed by its offer, not by a rule ──────────
ALTER TABLE subscription_changes DROP CONSTRAINT subscription_changes_change_kind_check;
ALTER TABLE subscription_changes ADD CONSTRAINT subscription_changes_change_kind_check
    CHECK (change_kind IN ('PLAN_CHANGE', 'QUANTITY_CHANGE', 'ADD_ON_CHANGE', 'PRICE_MIGRATION'));
ALTER TABLE subscription_changes ALTER COLUMN rule_id DROP NOT NULL;
ALTER TABLE subscription_changes ADD COLUMN migration_offer_id TEXT REFERENCES price_migration_offers (migration_offer_id);
ALTER TABLE subscription_changes ADD CONSTRAINT subscription_changes_one_basis CHECK (
    (rule_id IS NULL) <> (migration_offer_id IS NULL)
    AND (migration_offer_id IS NULL) = (change_kind <> 'PRICE_MIGRATION'));

ALTER TABLE subscription_versions DROP CONSTRAINT subscription_versions_change_type_check;
ALTER TABLE subscription_versions ADD CONSTRAINT subscription_versions_change_type_check
    CHECK (change_type IN ('STARTED', 'TRIAL_CONVERSION', 'TRIAL_EXPIRY', 'ACTIVATED',
                           'CANCELLATION_SCHEDULED', 'CANCELLATION_EFFECTIVE', 'CANCELED_NOW',
                           'REACTIVATED', 'TERM_EXPIRY', 'PLAN_CHANGED', 'QUANTITY_CHANGED', 'ADD_ON_CHANGED',
                           'PRICE_MIGRATED'));
