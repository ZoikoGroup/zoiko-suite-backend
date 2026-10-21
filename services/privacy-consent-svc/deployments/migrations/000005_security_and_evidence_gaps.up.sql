-- Migration 000005: security and evidence gaps found on the PRV-02
-- documentation-compliance audit.
--
-- Closes, inside this service's own boundary:
--   * no evidence of who PUBLISHED a notice version (only who created and
--     who approved it were ever recorded) — notice_versions
--   * the §10.1 "effective window" had no end, only a start — notice_versions
--   * ConsentRequest (§10.1's own documented evidence object) was accepted
--     on the wire and discarded before the INSERT — new consent_requests
--   * WithdrawalReceipt had no §10.1 "downstream propagation correlation
--     IDs" field — withdrawal_receipts
--   * ConsentReceipt had no §10.1 "receipt integrity", no §11.1 minor/
--     age-band evidence, and no §11.1 offline/imported-consent provenance —
--     consent_receipts
--   * no time-boxed (EXPIRED) or platform-initiated (INVALIDATED) consent
--     state existed, only subject-initiated WITHDRAWN — consent_receipts +
--     new consent_invalidations
--   * events could be silently dropped on a Kafka failure (the old
--     publisher logged "event dropped" and returned nil) — new
--     event_outbox, written in the SAME transaction as the evidence it
--     reports, delivered by a relay that retries until it succeeds

-- ── notice_versions: publisher evidence + effective window end ─────────────

ALTER TABLE notice_versions
    ADD COLUMN IF NOT EXISTS published_by_principal_id TEXT,
    ADD COLUMN IF NOT EXISTS published_at               TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS effective_to                TIMESTAMPTZ;

-- ── consent_receipts: integrity, age-band, offline-import, expiry ──────────

ALTER TABLE consent_receipts
    ADD COLUMN IF NOT EXISTS receipt_integrity_hash  TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS age_band                VARCHAR(32),
    ADD COLUMN IF NOT EXISTS is_offline_import        BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS import_source            TEXT,
    ADD COLUMN IF NOT EXISTS import_capture_method     TEXT,
    ADD COLUMN IF NOT EXISTS import_evidence_ref       TEXT,
    ADD COLUMN IF NOT EXISTS imported_at               TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS expires_at                TIMESTAMPTZ;

-- receipt_integrity_hash's NOT NULL DEFAULT '' is deliberate rather than
-- nullable: a receipt with no integrity hash at all is indistinguishable
-- from one nobody checked, and every new row computes a real one (see
-- store.receiptIntegrityHash). An empty string is a visibly unbackfilled
-- historical row, not a silent gap.

-- ── withdrawal_receipts: propagation correlation ────────────────────────────

ALTER TABLE withdrawal_receipts
    ADD COLUMN IF NOT EXISTS propagation_correlation_id TEXT NOT NULL DEFAULT '';

-- ── consent_requests: §10.1's own documented evidence object ────────────────
--
-- One row per ConsentReceipt (UNIQUE), never more: a receipt is granted or
-- denied against exactly one request context. Append-only, same as every
-- other evidence table in this service.

CREATE TABLE IF NOT EXISTS consent_requests (
    consent_request_id     UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID,
    consent_receipt_id      UUID        NOT NULL UNIQUE REFERENCES consent_receipts(consent_receipt_id),
    scope                   TEXT,
    data_context            TEXT,
    recipient_context       TEXT,
    technology_context      TEXT,
    vendor                  TEXT,
    storage_lifetime        TEXT,
    jurisdiction_rule_ref   TEXT,
    required_status         VARCHAR(16),
    notice_version_id       UUID        REFERENCES notice_versions(notice_version_id),
    policy_package          TEXT,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_consent_requests_receipt ON consent_requests (consent_receipt_id);

ALTER TABLE consent_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE consent_requests FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consent_requests
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE OR REPLACE FUNCTION reject_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% is append-only evidence: % is never permitted', TG_TABLE_NAME, TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER consent_requests_append_only
    BEFORE UPDATE OR DELETE ON consent_requests
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

-- ── consent_invalidations: platform-initiated terminal state ────────────────
--
-- Mirrors withdrawal_receipts exactly, with a distinct actor/reason shape:
-- withdrawal is the subject acting; invalidation is the platform acting
-- (compromised channel, confirmed fraud). One per receipt, same as
-- withdrawal, enforced the same way.

CREATE TABLE IF NOT EXISTS consent_invalidations (
    consent_invalidation_id    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                   UUID,
    consent_receipt_id          UUID        NOT NULL REFERENCES consent_receipts(consent_receipt_id),
    invalidated_by_principal_id TEXT        NOT NULL,
    reason                      TEXT        NOT NULL,
    channel                     VARCHAR(64) NOT NULL,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_consent_invalidations_one_per_consent
    ON consent_invalidations (consent_receipt_id);

ALTER TABLE consent_invalidations ENABLE ROW LEVEL SECURITY;
ALTER TABLE consent_invalidations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON consent_invalidations
    FOR ALL
    USING (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id IS NULL OR tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE TRIGGER consent_invalidations_append_only
    BEFORE UPDATE OR DELETE ON consent_invalidations
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

-- ── event_outbox: durable, at-least-once event delivery ─────────────────────
--
-- Same shape as identity-context-svc's event_outbox (ZS-EVENT-001 §6/§7):
-- written in the SAME transaction as the evidence row it reports, so a
-- crash between "the write committed" and "Kafka got it" leaves the event
-- here rather than nowhere. The relay drains every tenant's rows from one
-- process and cannot set a single app.tenant_id, so it authenticates via
-- the named app.outbox_relay capability instead of a superuser bypass —
-- see the policy comment below.

CREATE TABLE IF NOT EXISTS event_outbox (
    outbox_event_id VARCHAR(255) PRIMARY KEY,
    event_type      VARCHAR(200) NOT NULL,
    tenant_id       VARCHAR(255) NOT NULL DEFAULT '',
    entity_id       VARCHAR(255) NOT NULL DEFAULT '',
    actor_id        VARCHAR(255) NOT NULL DEFAULT '',
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    payload         JSONB        NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at    TIMESTAMPTZ,
    attempts        INT          NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    last_error      TEXT,

    CONSTRAINT event_outbox_attempts_nonneg CHECK (attempts >= 0)
);

CREATE INDEX IF NOT EXISTS idx_event_outbox_pending
    ON event_outbox (next_attempt_at, created_at)
    WHERE published_at IS NULL;

ALTER TABLE event_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE event_outbox FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON event_outbox
    FOR ALL
    USING (
        tenant_id = COALESCE(current_setting('app.tenant_id', true), '')
        OR current_setting('app.outbox_relay', true) = 'true'
    )
    WITH CHECK (
        tenant_id = COALESCE(current_setting('app.tenant_id', true), '')
        OR current_setting('app.outbox_relay', true) = 'true'
    );
