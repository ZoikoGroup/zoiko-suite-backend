-- 006 ZS-SVC-T-001 XIC-03: Inbound Exchange & Canonicalization Gateway.
--
-- An inbound exchange is the durable record of one authenticated delivery from an external system: what arrived
-- (hashes and metadata, and the raw body only when policy allows and only for a bounded time), what the gateway
-- decided (validated, quarantined, rejected, duplicate) and what the owning domain answered. Transport acceptance
-- and business acceptance are separate columns, never one status.
--
-- What the database guarantees on its own:
--   * tenant isolation (RLS, FORCEd); a connection's exchanges are readable only by its tenant;
--   * the same external event cannot produce two live exchanges: (tenant, dedupe_key) is unique, and a duplicate
--     delivery is its own terminal row pointing at the original;
--   * an exchange's identity, body hash, source reference and contract pin never change; its canonical result is
--     fixed once validated, and changes only when a governed release re-runs it (attempt_no + 1);
--   * state moves only VALIDATED > HANDED_OFF | QUARANTINED, QUARANTINED > VALIDATED | REJECTED; DUPLICATE,
--     REJECTED and HANDED_OFF are terminal;
--   * VALIDATED carries a canonical hash and mapping lineage, HANDED_OFF a target, QUARANTINED a class and reason;
--   * the raw and canonical bodies are removed only after their retention date, by the purge worker;
--   * handoff targets are platform data, written only from the platform scope, activated by someone other than their
--     author, and never edited after creation;
--   * evidence is append-only; a quarantine release or dismissal is decided by someone else.

CREATE TABLE IF NOT EXISTS xic_handoff_targets (
    target_id          UUID         PRIMARY KEY,
    canonical_contract VARCHAR(128) NOT NULL,
    base_url           VARCHAR(512) NOT NULL,
    path               VARCHAR(256) NOT NULL,
    region             VARCHAR(32)  NOT NULL,
    legal_entity_pointer VARCHAR(128) NOT NULL DEFAULT '',
    status             VARCHAR(10)  NOT NULL DEFAULT 'DRAFT',
    fingerprint        VARCHAR(64)  NOT NULL,
    created_by         VARCHAR(255) NOT NULL,
    approved_by        VARCHAR(255) NOT NULL DEFAULT '',
    approved_at        TIMESTAMPTZ,
    version            BIGINT       NOT NULL DEFAULT 1,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_target_status_valid CHECK (status IN ('DRAFT', 'ACTIVE', 'SUSPENDED', 'RETIRED')),
    CONSTRAINT xic_target_url_clean CHECK (base_url ~ '^https?://[^/@?#]+(/[^?#@]*)?$' AND path ~ '^/[^?#@ ]*$'),
    CONSTRAINT xic_target_active_approved CHECK (status = 'DRAFT' OR (approved_by <> '' AND approved_by <> created_by AND approved_at IS NOT NULL)),
    CONSTRAINT xic_target_version_positive CHECK (version >= 1)
);
CREATE UNIQUE INDEX IF NOT EXISTS xic_target_one_live ON xic_handoff_targets (canonical_contract) WHERE status IN ('DRAFT', 'ACTIVE', 'SUSPENDED');

CREATE OR REPLACE FUNCTION xic_target_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'handoff targets are never deleted; retire them' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.target_id <> OLD.target_id OR NEW.canonical_contract <> OLD.canonical_contract OR NEW.base_url <> OLD.base_url
       OR NEW.path <> OLD.path OR NEW.region <> OLD.region OR NEW.legal_entity_pointer <> OLD.legal_entity_pointer OR NEW.fingerprint <> OLD.fingerprint OR NEW.created_by <> OLD.created_by THEN
        RAISE EXCEPTION 'a handoff target is immutable; create a new one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'target version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
           (OLD.status = 'DRAFT'     AND NEW.status IN ('ACTIVE', 'RETIRED'))
        OR (OLD.status = 'ACTIVE'    AND NEW.status IN ('SUSPENDED', 'RETIRED'))
        OR (OLD.status = 'SUSPENDED' AND NEW.status IN ('ACTIVE', 'RETIRED'))) THEN
        RAISE EXCEPTION 'target status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_target_guard_trg ON xic_handoff_targets;
CREATE TRIGGER xic_target_guard_trg BEFORE UPDATE OR DELETE ON xic_handoff_targets FOR EACH ROW EXECUTE FUNCTION xic_target_guard();

CREATE TABLE IF NOT EXISTS xic_inbound_exchanges (
    exchange_id          UUID         PRIMARY KEY,
    tenant_id            VARCHAR(64)  NOT NULL,
    connection_id        UUID         NOT NULL REFERENCES xic_connections(connection_id),
    connector_id         VARCHAR(63)  NOT NULL,
    operation_id         VARCHAR(63)  NOT NULL,
    contract_version_id  UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    state                VARCHAR(12)  NOT NULL,
    domain_outcome       VARCHAR(14)  NOT NULL DEFAULT 'NOT_APPLICABLE',
    quarantine_class     VARCHAR(20),
    reason_code          VARCHAR(64)  NOT NULL DEFAULT '',
    xic_code             VARCHAR(8)   NOT NULL DEFAULT '',
    original_exchange_id UUID         REFERENCES xic_inbound_exchanges(exchange_id),
    dedupe_key           VARCHAR(64),
    source_ref_hash      VARCHAR(64)  NOT NULL DEFAULT '',
    body_hash            VARCHAR(64)  NOT NULL,
    body_size            INTEGER      NOT NULL,
    content_type         VARCHAR(128) NOT NULL DEFAULT '',
    source_schema        VARCHAR(80)  NOT NULL DEFAULT '',
    mapping_version_id   UUID         REFERENCES xic_mapping_versions(mapping_version_id),
    canonical_contract   VARCHAR(128) NOT NULL DEFAULT '',
    canonical_hash       VARCHAR(64)  NOT NULL DEFAULT '',
    lineage              JSONB,
    raw                  BYTEA,
    canonical            BYTEA,
    raw_expires_at       TIMESTAMPTZ,
    target_id            UUID         REFERENCES xic_handoff_targets(target_id),
    handoff_attempts     INTEGER      NOT NULL DEFAULT 0,
    next_attempt_at      TIMESTAMPTZ,
    last_error_code      VARCHAR(64)  NOT NULL DEFAULT '',
    domain_ref           VARCHAR(128) NOT NULL DEFAULT '',
    domain_status        INTEGER      NOT NULL DEFAULT 0,
    auth_mechanism       VARCHAR(24)  NOT NULL DEFAULT '',
    source_net_hash      VARCHAR(64)  NOT NULL DEFAULT '',
    correlation_id       VARCHAR(128) NOT NULL DEFAULT '',
    attempt_no           INTEGER      NOT NULL DEFAULT 1,
    version              BIGINT       NOT NULL DEFAULT 1,
    received_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_ix_dedupe UNIQUE (tenant_id, dedupe_key),
    CONSTRAINT xic_ix_state_valid CHECK (state IN ('RECEIVED', 'AUTHENTICATED', 'DUPLICATE', 'VALIDATED', 'QUARANTINED', 'HANDED_OFF', 'REJECTED')),
    CONSTRAINT xic_ix_domain_valid CHECK (domain_outcome IN ('NOT_APPLICABLE', 'PENDING', 'ACCEPTED', 'REJECTED', 'UNKNOWN')),
    CONSTRAINT xic_ix_class_valid CHECK (quarantine_class IS NULL OR quarantine_class IN
        ('SCHEMA', 'MAPPING', 'AUTH_CONTEXT', 'DATA_POLICY', 'DOMAIN_PRECONDITION', 'IDEMPOTENCY')),
    CONSTRAINT xic_ix_body_hash CHECK (body_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_ix_quarantined_classed CHECK (state <> 'QUARANTINED' OR (quarantine_class IS NOT NULL AND reason_code <> '')),
    CONSTRAINT xic_ix_rejected_reasoned CHECK (state <> 'REJECTED' OR reason_code <> ''),
    CONSTRAINT xic_ix_duplicate_points_back CHECK (state <> 'DUPLICATE' OR (original_exchange_id IS NOT NULL AND dedupe_key IS NULL)),
    CONSTRAINT xic_ix_validated_canonical CHECK (state NOT IN ('VALIDATED', 'HANDED_OFF')
        OR (canonical_hash ~ '^[0-9a-f]{64}$' AND mapping_version_id IS NOT NULL AND canonical_contract <> '')),
    CONSTRAINT xic_ix_handed_off_targeted CHECK (state <> 'HANDED_OFF' OR (target_id IS NOT NULL AND domain_outcome = 'ACCEPTED')),
    CONSTRAINT xic_ix_raw_dated CHECK (raw IS NULL OR raw_expires_at IS NOT NULL),
    CONSTRAINT xic_ix_attempts_nonneg CHECK (handoff_attempts >= 0 AND attempt_no >= 1 AND version >= 1)
);
CREATE INDEX IF NOT EXISTS xic_ix_conn ON xic_inbound_exchanges (connection_id, received_at);
CREATE INDEX IF NOT EXISTS xic_ix_handoff_due ON xic_inbound_exchanges (next_attempt_at) WHERE state = 'VALIDATED';
CREATE INDEX IF NOT EXISTS xic_ix_quarantined ON xic_inbound_exchanges (tenant_id, received_at) WHERE state = 'QUARANTINED';
CREATE INDEX IF NOT EXISTS xic_ix_purge ON xic_inbound_exchanges (raw_expires_at) WHERE raw IS NOT NULL OR canonical IS NOT NULL;

CREATE OR REPLACE FUNCTION xic_ix_guard() RETURNS trigger AS $$
DECLARE
    purge BOOLEAN := COALESCE(current_setting('app.xic_purge', true), '') = 'true';
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'an inbound exchange is never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.exchange_id <> OLD.exchange_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.connection_id <> OLD.connection_id
       OR NEW.connector_id <> OLD.connector_id OR NEW.operation_id <> OLD.operation_id OR NEW.contract_version_id <> OLD.contract_version_id
       OR NEW.body_hash <> OLD.body_hash OR NEW.source_ref_hash <> OLD.source_ref_hash OR NEW.dedupe_key IS DISTINCT FROM OLD.dedupe_key
       OR NEW.original_exchange_id IS DISTINCT FROM OLD.original_exchange_id OR NEW.received_at <> OLD.received_at THEN
        RAISE EXCEPTION 'the identity and body hash of an exchange are immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 AND NOT purge THEN
        RAISE EXCEPTION 'exchange version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF purge THEN
        -- the purge worker only removes bodies whose retention has passed, and changes nothing else
        IF OLD.raw_expires_at IS NULL OR OLD.raw_expires_at > now()
           OR NEW.state <> OLD.state OR NEW.canonical_hash <> OLD.canonical_hash OR NEW.domain_outcome <> OLD.domain_outcome
           OR (NEW.raw IS NOT NULL AND NEW.raw IS DISTINCT FROM OLD.raw) OR (NEW.canonical IS NOT NULL AND NEW.canonical IS DISTINCT FROM OLD.canonical) THEN
            RAISE EXCEPTION 'the purge worker removes expired bodies only' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF OLD.state <> NEW.state AND NOT (
           (OLD.state = 'VALIDATED'   AND NEW.state IN ('HANDED_OFF', 'QUARANTINED'))
        OR (OLD.state = 'QUARANTINED' AND NEW.state IN ('VALIDATED', 'REJECTED'))) THEN
        RAISE EXCEPTION 'exchange state cannot go from % to %', OLD.state, NEW.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.state IN ('HANDED_OFF', 'REJECTED', 'DUPLICATE') THEN
        RAISE EXCEPTION 'a % exchange is final', OLD.state USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- the canonical result is fixed once validated; only a governed release (a new attempt) may recompute it
    IF NEW.attempt_no = OLD.attempt_no AND OLD.canonical_hash <> '' AND
       (NEW.canonical_hash <> OLD.canonical_hash OR NEW.mapping_version_id IS DISTINCT FROM OLD.mapping_version_id
        OR NEW.canonical_contract <> OLD.canonical_contract OR NEW.source_schema <> OLD.source_schema) THEN
        RAISE EXCEPTION 'the canonical result of an attempt is fixed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.attempt_no <> OLD.attempt_no AND (NEW.attempt_no <> OLD.attempt_no + 1 OR OLD.state <> 'QUARANTINED') THEN
        RAISE EXCEPTION 'a new attempt follows a quarantined one, one at a time' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF (NEW.raw IS DISTINCT FROM OLD.raw AND OLD.raw IS NOT NULL) THEN
        RAISE EXCEPTION 'the raw payload is retained or purged, never rewritten' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_ix_guard_trg ON xic_inbound_exchanges;
CREATE TRIGGER xic_ix_guard_trg BEFORE UPDATE OR DELETE ON xic_inbound_exchanges FOR EACH ROW EXECUTE FUNCTION xic_ix_guard();

CREATE TABLE IF NOT EXISTS xic_inbound_events (
    ord         BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    exchange_id UUID         NOT NULL REFERENCES xic_inbound_exchanges(exchange_id),
    tenant_id   VARCHAR(64)  NOT NULL,
    kind        VARCHAR(40)  NOT NULL,
    from_state  VARCHAR(12)  NOT NULL DEFAULT '',
    to_state    VARCHAR(12)  NOT NULL DEFAULT '',
    detail      JSONB,
    actor       VARCHAR(255) NOT NULL,
    at          TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS xic_ix_events ON xic_inbound_events (exchange_id, ord);
DROP TRIGGER IF EXISTS xic_inbound_events_no_change ON xic_inbound_events;
CREATE TRIGGER xic_inbound_events_no_change BEFORE UPDATE OR DELETE ON xic_inbound_events FOR EACH ROW EXECUTE FUNCTION xic_append_only();

CREATE TABLE IF NOT EXISTS xic_quarantine_requests (
    request_id   UUID         PRIMARY KEY,
    exchange_id  UUID         NOT NULL REFERENCES xic_inbound_exchanges(exchange_id),
    tenant_id    VARCHAR(64)  NOT NULL,
    action       VARCHAR(8)   NOT NULL,
    reason       VARCHAR(255) NOT NULL,
    status       VARCHAR(10)  NOT NULL DEFAULT 'PENDING',
    requested_by VARCHAR(255) NOT NULL,
    decided_by   VARCHAR(255),
    decided_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_qr_action_valid CHECK (action IN ('RELEASE', 'DISMISS')),
    CONSTRAINT xic_qr_status_valid CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'USED')),
    CONSTRAINT xic_qr_distinct_people CHECK (decided_by IS NULL OR decided_by <> requested_by),
    CONSTRAINT xic_qr_decided_has_actor CHECK (status = 'PENDING' OR decided_by IS NOT NULL),
    CONSTRAINT xic_qr_reasoned CHECK (btrim(reason) <> '')
);
CREATE UNIQUE INDEX IF NOT EXISTS xic_qr_one_open ON xic_quarantine_requests (exchange_id) WHERE status IN ('PENDING', 'APPROVED');

CREATE OR REPLACE FUNCTION xic_qr_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'requests are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.request_id <> OLD.request_id OR NEW.exchange_id <> OLD.exchange_id OR NEW.action <> OLD.action
       OR NEW.reason <> OLD.reason OR NEW.requested_by <> OLD.requested_by THEN
        RAISE EXCEPTION 'a request is immutable apart from its decision' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'PENDING' AND NOT (OLD.status = 'APPROVED' AND NEW.status = 'USED') AND NEW.status <> OLD.status THEN
        RAISE EXCEPTION 'a decided request does not change again' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_qr_guard_trg ON xic_quarantine_requests;
CREATE TRIGGER xic_qr_guard_trg BEFORE UPDATE OR DELETE ON xic_quarantine_requests FOR EACH ROW EXECUTE FUNCTION xic_qr_guard();

-- ── row-level security ──────────────────────────────────────────────────────

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['xic_inbound_exchanges', 'xic_inbound_events', 'xic_quarantine_requests']
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
-- the purge worker may UPDATE exchanges across tenants, and the guard trigger limits what that can change
DROP POLICY IF EXISTS xic_inbound_exchanges_purge ON xic_inbound_exchanges;
CREATE POLICY xic_inbound_exchanges_purge ON xic_inbound_exchanges FOR UPDATE
    USING (COALESCE(NULLIF(current_setting('app.xic_purge', true), ''), 'false') = 'true')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.xic_purge', true), ''), 'false') = 'true');

-- handoff targets: platform data. Every tenant's worker must READ the active target to deliver; only the platform
-- scope writes (and the service never returns a target's address to a tenant).
ALTER TABLE xic_handoff_targets ENABLE ROW LEVEL SECURITY;
ALTER TABLE xic_handoff_targets FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS xic_target_read ON xic_handoff_targets;
CREATE POLICY xic_target_read ON xic_handoff_targets FOR SELECT USING (true);
DROP POLICY IF EXISTS xic_target_insert ON xic_handoff_targets;
CREATE POLICY xic_target_insert ON xic_handoff_targets FOR INSERT WITH CHECK (xic_is_platform());
DROP POLICY IF EXISTS xic_target_update ON xic_handoff_targets;
CREATE POLICY xic_target_update ON xic_handoff_targets FOR UPDATE USING (xic_is_platform()) WITH CHECK (xic_is_platform());
