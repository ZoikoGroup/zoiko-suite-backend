-- 007 ZS-SVC-T-001 XIC-04: Outbound Delivery (durable attempts and ambiguity resolution).
--
-- An outbound attempt is the durable record of ONE logical side effect at an external provider. It is written
-- before any network call (I-13), and a timeout or lost answer after the call may have reached the provider leaves
-- it UNKNOWN until the ORIGINAL attempt is resolved (I-14): there is no second logical attempt and no blind resend.
--
-- What the database guarantees on its own:
--   * tenant isolation (RLS, FORCEd);
--   * one logical operation per (tenant, connection, operation, domain request): reserving it again returns the
--     original, never a second row;
--   * identity, payload hash, provider idempotency reference hash and contract pin never change;
--   * state moves only along the documented graph; SETTLED, FAILED and CANCELLED are final;
--   * entering SUBMITTING advances submit_count by exactly one and stamps last_submit_at, and an attempt that is
--     UNKNOWN or PENDING can be submitted again only if the contract made resubmission safe (resubmit_allowed,
--     fixed at reservation) - so the engine cannot, even by a bug, send a second blind duplicate;
--   * UNKNOWN names its ambiguity; a terminal attempt carries its final outcome and how it was reached;
--   * a manual resolution names evidence and is made by someone other than whoever reserved the attempt;
--   * the stored request body (kept only while the attempt is open, and only when data policy allows) is removed,
--     never rewritten; the purge worker removes it once the attempt is final or its retention has passed;
--   * evidence is append-only.

CREATE TABLE IF NOT EXISTS xic_outbound_attempts (
    attempt_id               UUID         PRIMARY KEY,
    tenant_id                VARCHAR(64)  NOT NULL,
    connection_id            UUID         NOT NULL REFERENCES xic_connections(connection_id),
    connector_id             VARCHAR(63)  NOT NULL,
    operation_id             VARCHAR(63)  NOT NULL,
    contract_version_id      UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    domain_request_ref       VARCHAR(128) NOT NULL,
    payload_profile          VARCHAR(80)  NOT NULL DEFAULT '',
    payload_hash             VARCHAR(64)  NOT NULL,
    payload_size             INTEGER      NOT NULL,
    provider_idem_ref_hash   VARCHAR(64)  NOT NULL DEFAULT '',
    resubmit_allowed         BOOLEAN      NOT NULL DEFAULT false,
    state                    VARCHAR(10)  NOT NULL DEFAULT 'RESERVED',
    ambiguity_reason         VARCHAR(64)  NOT NULL DEFAULT '',
    provider_reference       VARCHAR(128) NOT NULL DEFAULT '',
    provider_status          INTEGER      NOT NULL DEFAULT 0,
    submit_count             INTEGER      NOT NULL DEFAULT 0,
    last_submit_at           TIMESTAMPTZ,
    resolve_count            INTEGER      NOT NULL DEFAULT 0,
    next_resolution_at       TIMESTAMPTZ,
    escalated_at             TIMESTAMPTZ,
    resolution_method        VARCHAR(16)  NOT NULL DEFAULT '',
    resolved_by              VARCHAR(255) NOT NULL DEFAULT '',
    evidence_ref             VARCHAR(255) NOT NULL DEFAULT '',
    final_outcome            JSONB,
    body                     BYTEA,
    body_expires_at          TIMESTAMPTZ,
    correlation_id           VARCHAR(128) NOT NULL DEFAULT '',
    requested_by             VARCHAR(255) NOT NULL,
    version                  BIGINT       NOT NULL DEFAULT 1,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_oa_logical UNIQUE (tenant_id, connection_id, operation_id, domain_request_ref),
    CONSTRAINT xic_oa_state_valid CHECK (state IN ('RESERVED', 'SUBMITTING', 'PENDING', 'UNKNOWN', 'SETTLED', 'FAILED', 'CANCELLED')),
    CONSTRAINT xic_oa_hash CHECK (payload_hash ~ '^[0-9a-f]{64}$' AND (provider_idem_ref_hash = '' OR provider_idem_ref_hash ~ '^[0-9a-f]{64}$')),
    CONSTRAINT xic_oa_counts CHECK (submit_count >= 0 AND resolve_count >= 0 AND version >= 1 AND payload_size >= 0),
    CONSTRAINT xic_oa_submitting_stamped CHECK (state <> 'SUBMITTING' OR (submit_count >= 1 AND last_submit_at IS NOT NULL)),
    CONSTRAINT xic_oa_unknown_named CHECK (state <> 'UNKNOWN' OR ambiguity_reason <> ''),
    CONSTRAINT xic_oa_final_outcome CHECK (state NOT IN ('SETTLED', 'FAILED') OR (final_outcome IS NOT NULL AND resolution_method <> '')),
    CONSTRAINT xic_oa_method_valid CHECK (resolution_method IN ('', 'SUBMIT', 'STATUS_QUERY', 'CALLBACK', 'RECONCILIATION', 'MANUAL')),
    CONSTRAINT xic_oa_manual_governed CHECK (resolution_method <> 'MANUAL' OR (resolved_by <> '' AND resolved_by <> requested_by AND btrim(evidence_ref) <> '')),
    CONSTRAINT xic_oa_body_dated CHECK (body IS NULL OR body_expires_at IS NOT NULL),
    CONSTRAINT xic_oa_open_body CHECK (body IS NULL OR state IN ('RESERVED', 'SUBMITTING', 'PENDING', 'UNKNOWN'))
);
CREATE INDEX IF NOT EXISTS xic_oa_conn ON xic_outbound_attempts (connection_id, created_at);
CREATE INDEX IF NOT EXISTS xic_oa_resolution_due ON xic_outbound_attempts (next_resolution_at) WHERE state IN ('SUBMITTING', 'PENDING', 'UNKNOWN');
CREATE INDEX IF NOT EXISTS xic_oa_purge ON xic_outbound_attempts (body_expires_at) WHERE body IS NOT NULL;

CREATE OR REPLACE FUNCTION xic_oa_guard() RETURNS trigger AS $$
DECLARE
    purge BOOLEAN := COALESCE(current_setting('app.xic_purge', true), '') = 'true';
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an outbound attempt is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.attempt_id <> OLD.attempt_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.connection_id <> OLD.connection_id
       OR NEW.connector_id <> OLD.connector_id OR NEW.operation_id <> OLD.operation_id OR NEW.contract_version_id <> OLD.contract_version_id
       OR NEW.domain_request_ref <> OLD.domain_request_ref OR NEW.payload_hash <> OLD.payload_hash OR NEW.payload_size <> OLD.payload_size
       OR NEW.provider_idem_ref_hash <> OLD.provider_idem_ref_hash OR NEW.resubmit_allowed <> OLD.resubmit_allowed
       OR NEW.requested_by <> OLD.requested_by OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'the identity and payload hash of an outbound attempt are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF purge THEN
        -- the purge worker removes a stored body that is no longer needed, and changes nothing else
        IF OLD.body IS NULL OR NEW.body IS NOT NULL OR NEW.state <> OLD.state
           OR NOT (OLD.state IN ('SETTLED', 'FAILED', 'CANCELLED') OR OLD.body_expires_at <= now()) THEN
            RAISE EXCEPTION 'the purge worker removes unneeded bodies only' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'attempt version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('SETTLED', 'FAILED', 'CANCELLED') THEN
        RAISE EXCEPTION 'a % attempt is final', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state <> NEW.state AND NOT (
           (OLD.state = 'RESERVED'   AND NEW.state IN ('SUBMITTING', 'CANCELLED'))
        OR (OLD.state = 'SUBMITTING' AND NEW.state IN ('RESERVED', 'PENDING', 'UNKNOWN', 'SETTLED', 'FAILED'))
        OR (OLD.state = 'PENDING'    AND NEW.state IN ('SUBMITTING', 'UNKNOWN', 'SETTLED', 'FAILED'))
        OR (OLD.state = 'UNKNOWN'    AND NEW.state IN ('SUBMITTING', 'PENDING', 'SETTLED', 'FAILED'))) THEN
        RAISE EXCEPTION 'attempt state cannot go from % to %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- a second submission of an open attempt is possible only where the contract made it safe (NP-20 / NP-21)
    IF NEW.state = 'SUBMITTING' AND OLD.state IN ('PENDING', 'UNKNOWN') AND NOT OLD.resubmit_allowed THEN
        RAISE EXCEPTION 'this attempt may not be submitted again: resolve the original' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.state = 'SUBMITTING' AND OLD.state <> 'SUBMITTING' AND NEW.submit_count <> OLD.submit_count + 1 THEN
        RAISE EXCEPTION 'submitting advances submit_count by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NOT (NEW.state = 'SUBMITTING' AND OLD.state <> 'SUBMITTING') AND NEW.submit_count <> OLD.submit_count THEN
        RAISE EXCEPTION 'submit_count changes only when a submission starts' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.body IS DISTINCT FROM OLD.body AND NEW.body IS NOT NULL THEN
        RAISE EXCEPTION 'the stored body is kept or removed, never rewritten' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_oa_guard_trg ON xic_outbound_attempts;
CREATE TRIGGER xic_oa_guard_trg BEFORE UPDATE OR DELETE ON xic_outbound_attempts FOR EACH ROW EXECUTE FUNCTION xic_oa_guard();

CREATE TABLE IF NOT EXISTS xic_outbound_events (
    ord         BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    attempt_id  UUID         NOT NULL REFERENCES xic_outbound_attempts(attempt_id),
    tenant_id   VARCHAR(64)  NOT NULL,
    kind        VARCHAR(40)  NOT NULL,
    from_state  VARCHAR(10)  NOT NULL DEFAULT '',
    to_state    VARCHAR(10)  NOT NULL DEFAULT '',
    detail      JSONB,
    actor       VARCHAR(255) NOT NULL,
    at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS xic_oa_events ON xic_outbound_events (attempt_id, ord);
DROP TRIGGER IF EXISTS xic_outbound_events_no_change ON xic_outbound_events;
CREATE TRIGGER xic_outbound_events_no_change BEFORE UPDATE OR DELETE ON xic_outbound_events FOR EACH ROW EXECUTE FUNCTION xic_append_only();

-- ── row-level security ──────────────────────────────────────────────────────

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['xic_outbound_attempts', 'xic_outbound_events']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_tenant', t);
        EXECUTE format($p$CREATE POLICY %I ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
            WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))$p$, t || '_tenant', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_worker_read', t);
        EXECUTE format($p$CREATE POLICY %I ON %I FOR SELECT
            USING (COALESCE(NULLIF(current_setting('app.xic_worker', true), ''), 'false') = 'true')$p$, t || '_worker_read', t);
    END LOOP;
END $$;
-- the purge worker may UPDATE attempts across tenants, and the guard trigger limits what that can change
DROP POLICY IF EXISTS xic_outbound_attempts_purge ON xic_outbound_attempts;
CREATE POLICY xic_outbound_attempts_purge ON xic_outbound_attempts FOR UPDATE
    USING (COALESCE(NULLIF(current_setting('app.xic_purge', true), ''), 'false') = 'true')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.xic_purge', true), ''), 'false') = 'true');
