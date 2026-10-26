-- 008 ZS-SVC-T-001 XIC-04: provider callbacks (callback receipts).
--
-- A callback receipt is the independent evidence of one provider callback: what arrived (hashes and metadata,
-- never the body), whether the sender was authenticated, what the callback claimed and what happened to the attempt.
-- Provider callback validation is independent of attempt state (§4): a receipt exists whether or not the attempt
-- moved.
--
-- What the database guarantees on its own:
--   * tenant isolation (RLS, FORCEd);
--   * a receipt is written once and never changes or disappears (append-only);
--   * the same provider event cannot be APPLIED twice: (tenant, dedupe_key) is unique, and a repeat is its own
--     terminal DUPLICATE row pointing at the original (or naming that the outcome was already recorded);
--   * an APPLIED receipt names the attempt it applied to, the state it found and the state it left;
--   * a receipt that flags an inconsistency (a late callback contradicting a recorded outcome) says why, so it can
--     be found by reconciliation.

CREATE TABLE IF NOT EXISTS xic_callback_receipts (
    receipt_id            UUID         PRIMARY KEY,
    tenant_id             VARCHAR(64)  NOT NULL,
    connection_id         UUID         NOT NULL REFERENCES xic_connections(connection_id),
    connector_id          VARCHAR(63)  NOT NULL,
    operation_id          VARCHAR(63)  NOT NULL,
    contract_version_id   UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    attempt_id            UUID         REFERENCES xic_outbound_attempts(attempt_id),
    state                 VARCHAR(13)  NOT NULL,
    auth_result           VARCHAR(12)  NOT NULL,
    auth_mechanism        VARCHAR(24)  NOT NULL DEFAULT '',
    key_label             VARCHAR(12)  NOT NULL DEFAULT '',
    reason_code           VARCHAR(64)  NOT NULL DEFAULT '',
    xic_code              VARCHAR(8)   NOT NULL DEFAULT '',
    dedupe_key            VARCHAR(64),
    original_receipt_id   UUID         REFERENCES xic_callback_receipts(receipt_id),
    body_hash             VARCHAR(64)  NOT NULL,
    body_size             INTEGER      NOT NULL,
    provider_reference    VARCHAR(128) NOT NULL DEFAULT '',
    reported_status       VARCHAR(40)  NOT NULL DEFAULT '',
    target_state          VARCHAR(10)  NOT NULL DEFAULT '',
    attempt_state_before  VARCHAR(10)  NOT NULL DEFAULT '',
    attempt_state_after   VARCHAR(10)  NOT NULL DEFAULT '',
    needs_reconciliation  BOOLEAN      NOT NULL DEFAULT false,
    source_net_hash       VARCHAR(64)  NOT NULL DEFAULT '',
    correlation_id        VARCHAR(128) NOT NULL DEFAULT '',
    received_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_cr_dedupe UNIQUE (tenant_id, dedupe_key),
    CONSTRAINT xic_cr_state_valid CHECK (state IN ('RECEIVED', 'AUTHENTICATED', 'DUPLICATE', 'APPLIED', 'REJECTED')),
    CONSTRAINT xic_cr_auth_valid CHECK (auth_result IN ('OK', 'FAILED', 'REPLAY', 'UNSUPPORTED')),
    CONSTRAINT xic_cr_hash CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_cr_rejected_reasoned CHECK (state <> 'REJECTED' OR reason_code <> ''),
    CONSTRAINT xic_cr_applied_attempted CHECK (state <> 'APPLIED' OR (attempt_id IS NOT NULL AND attempt_state_before <> '' AND attempt_state_after <> '' AND target_state <> '' AND auth_result = 'OK')),
    CONSTRAINT xic_cr_applied_keyed CHECK (state <> 'APPLIED' OR dedupe_key IS NOT NULL),
    CONSTRAINT xic_cr_duplicate_points_back CHECK (state <> 'DUPLICATE' OR (dedupe_key IS NULL AND (original_receipt_id IS NOT NULL OR reason_code = 'OUTCOME_ALREADY_RECORDED'))),
    CONSTRAINT xic_cr_reconcile_reasoned CHECK (NOT needs_reconciliation OR (state = 'REJECTED' AND reason_code = 'LATE_CONTRADICTS_FINAL' AND attempt_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS xic_cr_conn ON xic_callback_receipts (connection_id, received_at);
CREATE INDEX IF NOT EXISTS xic_cr_attempt ON xic_callback_receipts (attempt_id, received_at) WHERE attempt_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS xic_cr_reconcile ON xic_callback_receipts (tenant_id, received_at) WHERE needs_reconciliation;

-- find an attempt by the provider's reference or the request reference a callback echoes
CREATE INDEX IF NOT EXISTS xic_oa_provider_ref ON xic_outbound_attempts (connection_id, operation_id, provider_reference) WHERE provider_reference <> '';

DROP TRIGGER IF EXISTS xic_callback_receipts_no_change ON xic_callback_receipts;
CREATE TRIGGER xic_callback_receipts_no_change BEFORE UPDATE OR DELETE ON xic_callback_receipts FOR EACH ROW EXECUTE FUNCTION xic_append_only();

-- ── row-level security ──────────────────────────────────────────────────────

ALTER TABLE xic_callback_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE xic_callback_receipts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS xic_callback_receipts_tenant ON xic_callback_receipts;
CREATE POLICY xic_callback_receipts_tenant ON xic_callback_receipts FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
DROP POLICY IF EXISTS xic_callback_receipts_worker_read ON xic_callback_receipts;
CREATE POLICY xic_callback_receipts_worker_read ON xic_callback_receipts FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.xic_worker', true), ''), 'false') = 'true');
