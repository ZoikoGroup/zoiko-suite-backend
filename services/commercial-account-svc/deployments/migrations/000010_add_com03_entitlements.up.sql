-- 000010_add_com03_entitlements.up.sql
-- COM-03 Entitlement, part 3a (ZS-SVC-Q-001 §4.3; COM-CTRL-010, -011, -012,
-- -030; negative paths #08-#13, #39).
--
-- A decision is computed from immutable, effective-dated inputs — the price
-- versions a subscription is bound to, the subscription version in force,
-- the active restrictions and the ended-access policy version — never from a
-- stored grant. There is therefore no row anyone could write to give a
-- tenant a capability its subscription does not include (#39), and every
-- past decision can be reconstructed exactly.

-- ── Ended-access policy (seller plane) ────────────────────────────────────
--
-- What a customer may still do once its subscription has ended: nothing, or
-- read-only access for a stated number of days. Versioned and effective-
-- dated (COM-CTRL-012); a new version never rewrites what applied before it.
-- None is seeded: until ZoikoSuite publishes one, an ended subscription's
-- capabilities are denied.
CREATE TABLE entitlement_policy_versions (
    policy_version           INT          PRIMARY KEY CHECK (policy_version >= 1),
    ended_outcome            VARCHAR(16)  NOT NULL CHECK (ended_outcome IN ('READ_ONLY', 'DENY')),
    ended_read_only_days     INT          NOT NULL CHECK (ended_read_only_days >= 0),
    effective_from           TIMESTAMPTZ  NOT NULL,
    reason                   TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    created_at               TIMESTAMPTZ  NOT NULL,
    created_by_principal_id  VARCHAR(255) NOT NULL,
    CONSTRAINT entitlement_policy_days_only_for_read_only CHECK (
        (ended_outcome = 'READ_ONLY' AND ended_read_only_days >= 1)
        OR (ended_outcome = 'DENY' AND ended_read_only_days = 0))
);

CREATE FUNCTION reject_immutable_row() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable; publish a new version instead', TG_TABLE_NAME USING ERRCODE = 'CP001';
END;
$$;

CREATE TRIGGER trg_entitlement_policy_versions_immutable
    BEFORE UPDATE OR DELETE ON entitlement_policy_versions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

ALTER TABLE entitlement_policy_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE entitlement_policy_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY entitlement_policies_read ON entitlement_policy_versions FOR SELECT USING (true);
CREATE POLICY entitlement_policies_seller_insert ON entitlement_policy_versions FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- ── Commercial restrictions (tenant plane) ────────────────────────────────
--
-- A restriction lowers what a tenant may do — read-only, or restricted — for
-- a stated reason under a named policy (a dunning policy version, a legal
-- hold). It can only ever lower access: a decision takes the most severe of
-- the subscription result and every active restriction. It is applied once
-- and lifted once, and never edited.
CREATE TABLE commercial_restrictions (
    restriction_id           TEXT         PRIMARY KEY
        CHECK (restriction_id ~ '^crst_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    organization_id          UUID         NOT NULL,
    level                    VARCHAR(16)  NOT NULL CHECK (level IN ('READ_ONLY', 'RESTRICTED')),
    reason_code              VARCHAR(64)  NOT NULL CHECK (reason_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    policy_ref               VARCHAR(255) NOT NULL CHECK (btrim(policy_ref) <> ''),
    basis_ref                VARCHAR(255) NOT NULL CHECK (btrim(basis_ref) <> ''),
    applied_at               TIMESTAMPTZ  NOT NULL,
    applied_by_principal_id  VARCHAR(255) NOT NULL,
    lifted_at                TIMESTAMPTZ,
    lifted_by_principal_id   VARCHAR(255),
    lift_reason              TEXT,
    CONSTRAINT restrictions_lift_all_or_nothing CHECK (
        (lifted_at IS NULL) = (lifted_by_principal_id IS NULL)
        AND (lifted_at IS NULL) = (lift_reason IS NULL)
        AND (lift_reason IS NULL OR btrim(lift_reason) <> '')),
    CONSTRAINT restrictions_lift_after_apply CHECK (lifted_at IS NULL OR lifted_at >= applied_at)
);

CREATE INDEX idx_commercial_restrictions_active ON commercial_restrictions (organization_id) WHERE lifted_at IS NULL;

CREATE FUNCTION enforce_restriction_lift_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    lift_cols TEXT[] := ARRAY['lifted_at', 'lifted_by_principal_id', 'lift_reason'];
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'restriction % cannot be deleted; lift it', OLD.restriction_id USING ERRCODE = 'CP001';
    END IF;
    IF OLD.lifted_at IS NOT NULL THEN
        RAISE EXCEPTION 'restriction % is already lifted', OLD.restriction_id USING ERRCODE = 'CP001';
    END IF;
    IF (to_jsonb(NEW) - lift_cols) IS DISTINCT FROM (to_jsonb(OLD) - lift_cols) THEN
        RAISE EXCEPTION 'restriction % is never edited; lift it and apply a new one', OLD.restriction_id
            USING ERRCODE = 'CP001';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER trg_commercial_restrictions_lift_only
    BEFORE UPDATE OR DELETE ON commercial_restrictions
    FOR EACH ROW EXECUTE FUNCTION enforce_restriction_lift_only();

-- Applying and lifting a restriction is platform authority (never the
-- organization it applies to); reading one is the organization's own, or
-- the seller plane's, same shape as the price book and migration offers.
ALTER TABLE commercial_restrictions ENABLE ROW LEVEL SECURITY;
ALTER TABLE commercial_restrictions FORCE ROW LEVEL SECURITY;
CREATE POLICY restrictions_read ON commercial_restrictions FOR SELECT
    USING (current_setting('app.commercial_plane', true) = 'seller'
           OR organization_id::text = NULLIF(current_setting('app.tenant_id', true), ''));
CREATE POLICY restrictions_seller_insert ON commercial_restrictions FOR INSERT
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');
CREATE POLICY restrictions_seller_update ON commercial_restrictions FOR UPDATE
    USING (current_setting('app.commercial_plane', true) = 'seller')
    WITH CHECK (current_setting('app.commercial_plane', true) = 'seller');

-- A decision looks up the organization's subscription live at an instant.
CREATE INDEX idx_subscriptions_org_window ON subscriptions (organization_id, starts_at, ends_at);
