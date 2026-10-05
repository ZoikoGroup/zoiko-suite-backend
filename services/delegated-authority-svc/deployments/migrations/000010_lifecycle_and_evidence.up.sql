-- 000010: the ORG-06 lifecycle Proposed → Active → Suspended/Expired/Revoked,
-- the evidence §4.6 asks for, and one interval semantics everywhere.
--
-- Re-audit of 5 Oct 2026: PROPOSED and SUSPENDED existed only as Go constants
-- (this CHECK refused them); no creation reason and no revocation reason were
-- recorded at all, although the 23 Sep audit scored "revocation reason"
-- present; nothing recorded who approved a delegation; and three different
-- interval semantics were in use (expiry inclusive of effective_to, the
-- exclusion constraint closed, the overlap pre-check half-open), so two
-- back-to-back delegations passed the pre-check and then failed in the
-- database as a 503.

-- ── states ──────────────────────────────────────────────────────────────────
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_status_known;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_status_known
    CHECK (status IN ('PROPOSED', 'ACTIVE', 'SUSPENDED', 'REVOKED', 'EXPIRED')) NOT VALID;

-- ── evidence (§4.6: delegator, authority basis, scope, limits, valid time,
--    approval, revocation reason) ─────────────────────────────────────────────
ALTER TABLE delegation_grants
    ADD COLUMN IF NOT EXISTS reason                    TEXT,
    ADD COLUMN IF NOT EXISTS approved_by_principal_id  VARCHAR(255),
    ADD COLUMN IF NOT EXISTS approved_at               TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS approval_method           VARCHAR(30),
    ADD COLUMN IF NOT EXISTS suspended_by_principal_id VARCHAR(255),
    ADD COLUMN IF NOT EXISTS suspended_at              TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS suspension_reason         TEXT,
    ADD COLUMN IF NOT EXISTS revocation_reason         TEXT;

-- Grants that were ACTIVE before approval was recorded say so, rather than
-- inventing an approver.
UPDATE delegation_grants
   SET approval_method = 'LEGACY_UNRECORDED', approved_by_principal_id = created_by_principal_id, approved_at = created_at
 WHERE approval_method IS NULL AND status <> 'PROPOSED';

ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_approval_method_known;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_approval_method_known
    CHECK (approval_method IS NULL OR approval_method IN
        ('DELEGATOR_SELF', 'DELEGATOR_APPROVAL', 'ADMINISTRATOR_APPROVAL', 'LEGACY_UNRECORDED')) NOT VALID;

-- Anything that has ever been ACTIVE carries its approval; a PROPOSED grant
-- carries none yet.
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_approval_evidence;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_approval_evidence
    CHECK ((status = 'PROPOSED' AND approved_at IS NULL AND approved_by_principal_id IS NULL AND approval_method IS NULL)
           OR (status IN ('ACTIVE', 'SUSPENDED') AND approved_at IS NOT NULL AND approved_by_principal_id IS NOT NULL
               AND approval_method IS NOT NULL)
           OR status IN ('REVOKED', 'EXPIRED')) NOT VALID;

-- Maker-checker: an administrator approving a proposal is neither its maker
-- nor its beneficiary; the delegator approving is the delegator.
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_approval_segregated;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_approval_segregated
    CHECK (approval_method IS DISTINCT FROM 'ADMINISTRATOR_APPROVAL'
           OR (approved_by_principal_id <> created_by_principal_id AND approved_by_principal_id <> delegate_principal_id)) NOT VALID;
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_delegator_approval_is_delegator;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_delegator_approval_is_delegator
    CHECK (approval_method NOT IN ('DELEGATOR_SELF', 'DELEGATOR_APPROVAL') OR approval_method IS NULL
           OR approved_by_principal_id = delegator_principal_id) NOT VALID;

ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_suspended_has_evidence;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_suspended_has_evidence
    CHECK (status <> 'SUSPENDED'
           OR (suspended_at IS NOT NULL AND suspended_by_principal_id IS NOT NULL AND COALESCE(suspension_reason, '') <> '')) NOT VALID;

-- A revocation made from now on states why. Historic rows have no reason and
-- keep none: NOT VALID applies the rule to new writes only.
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_revocation_reason;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_revocation_reason
    CHECK (status <> 'REVOKED' OR COALESCE(revocation_reason, '') <> '') NOT VALID;

-- ── one interval semantics: [effective_from, effective_to) ─────────────────
-- effective_to is the first instant the authority no longer applies — what
-- authorization-svc's projection already uses (effective_to > now()) and what
-- expired_at records. Back-to-back grants are therefore not an overlap.
ALTER TABLE delegation_grants DROP CONSTRAINT IF EXISTS delegation_grants_no_overlapping_active;
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_no_overlapping_active
    EXCLUDE USING GIST (
        tenant_id WITH =,
        legal_entity_id WITH =,
        delegate_principal_id WITH =,
        action_type WITH =,
        tstzrange(effective_from, effective_to, '[)') WITH &&
    ) WHERE (status = 'ACTIVE');

-- ── append-only history (delegation history reconstructable; as-of reads) ──
CREATE TABLE IF NOT EXISTS delegation_history (
    history_id      BIGSERIAL    PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    delegation_id   UUID         NOT NULL,
    version         BIGINT       NOT NULL,
    transition      VARCHAR(20)  NOT NULL CHECK (transition IN
                        ('PROPOSED', 'ACTIVATED', 'SUSPENDED', 'RESUMED', 'EXTENDED', 'REVOKED', 'EXPIRED')),
    status          VARCHAR(20)  NOT NULL,
    effective_from  TIMESTAMPTZ  NOT NULL,
    effective_to    TIMESTAMPTZ  NOT NULL,
    actor_principal_id VARCHAR(255) NOT NULL,
    reason          TEXT,
    recorded_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (delegation_id, version)
);
CREATE INDEX IF NOT EXISTS idx_delegation_history_lookup ON delegation_history (tenant_id, delegation_id, version);
CREATE INDEX IF NOT EXISTS idx_delegation_history_asof ON delegation_history (tenant_id, recorded_at);

ALTER TABLE delegation_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE delegation_history FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS delegation_history_tenant_isolation ON delegation_history;
CREATE POLICY delegation_history_tenant_isolation ON delegation_history FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
-- The cross-tenant expiry sweeper records EXPIRED transitions under its own flag.
DROP POLICY IF EXISTS delegation_history_sweeper ON delegation_history;
CREATE POLICY delegation_history_sweeper ON delegation_history FOR INSERT
    WITH CHECK (COALESCE(NULLIF(current_setting('app.expiry_sweeper', true), ''), 'false') = 'true');

CREATE OR REPLACE FUNCTION delegation_history_append_only() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'delegation_history is append-only';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_delegation_history_append_only ON delegation_history;
CREATE TRIGGER trg_delegation_history_append_only
    BEFORE UPDATE OR DELETE ON delegation_history
    FOR EACH ROW EXECUTE FUNCTION delegation_history_append_only();

-- Seed the history of every existing grant with the state it is in now, so an
-- as-of read is never blind to a grant created before this migration.
INSERT INTO delegation_history (tenant_id, delegation_id, version, transition, status, effective_from, effective_to,
                                actor_principal_id, reason, recorded_at)
SELECT tenant_id, delegation_id, version,
       CASE status WHEN 'REVOKED' THEN 'REVOKED' WHEN 'EXPIRED' THEN 'EXPIRED' ELSE 'ACTIVATED' END,
       status, effective_from, effective_to, created_by_principal_id, 'seeded by migration 000010', created_at
  FROM delegation_grants
ON CONFLICT (delegation_id, version) DO NOTHING;

-- ── outbox vocabulary ───────────────────────────────────────────────────────
-- 000003 allowed only delegated/revoked/expired, so authority.extended — added
-- on 28 Sep — could never be written: every extend failed in the database.
ALTER TABLE delegation_outbox DROP CONSTRAINT IF EXISTS delegation_outbox_event_known;
ALTER TABLE delegation_outbox
    ADD CONSTRAINT delegation_outbox_event_known
    CHECK (event_type IN ('authority.delegated', 'authority.revoked', 'authority.expired',
                          'authority.extended', 'authority.suspended', 'authority.resumed'));

-- ── refusal reasons ─────────────────────────────────────────────────────────
ALTER TABLE refused_escalations DROP CONSTRAINT IF EXISTS refused_escalations_reason_known;
ALTER TABLE refused_escalations
    ADD CONSTRAINT refused_escalations_reason_known
    CHECK (refusal_reason IN ('self_dealing', 'delegator_mismatch', 'delegator_lacks_authority', 'sod_conflict',
                              'overlap_conflict', 'invalid_window', 'no_create_grant', 'delegate_is_delegator',
                              'delegator_exceeds_limit', 'invalid_limit', 'approval_not_segregated'));

-- ── Idempotency-Key replay store (cross-service finding 2) ─────────────────
-- Same shape as notification-svc and identity-context-svc: one row per
-- (tenant, endpoint, key); the fingerprint folds in the acting principal, so a
-- second principal replaying the first one's key gets a mismatch, not their
-- response. response_status 0 = claimed, in flight.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(255) NOT NULL,
    endpoint            VARCHAR(500) NOT NULL,
    idempotency_key     VARCHAR(255) NOT NULL,
    request_fingerprint TEXT         NOT NULL,
    response_status     INT          NOT NULL DEFAULT 0,
    response_body       JSONB        NOT NULL DEFAULT 'null'::jsonb,
    created_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, endpoint, idempotency_key)
);
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS idempotency_keys_tenant_isolation ON idempotency_keys;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
