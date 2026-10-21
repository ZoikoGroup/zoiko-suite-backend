-- ZS-SVC-M-001 INT-07 (Banking adapter). Makes the provider call real and safe:
--
--   * a certified, versioned PROVIDER PROFILE says how one bank/PSP is spoken to (platform reference data,
--     maker-checker certified, immutable once certified);
--   * an attempt now PINS the profile version, rail and connector it was first sent with, so a retry can
--     never be sent differently from the original;
--   * SUBMITTING is the durable "the request is leaving" state, set by compare-and-set BEFORE the network
--     call. A crash mid-call leaves SUBMITTING behind; a sweeper turns a stale one into PENDING_UNKNOWN,
--     never into a blind resend;
--   * every provider call is evidenced in an append-only transport log (hashes, not bodies, no secrets).
--
-- Re-runnable: CREATE ... IF NOT EXISTS and DROP ... IF EXISTS throughout.

-- ── attempt: pinned provider context and in-flight state ────────────────────
ALTER TABLE payment_initiation_attempts
    ADD COLUMN IF NOT EXISTS provider_key       TEXT         NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS profile_version    INTEGER      NULL,
    ADD COLUMN IF NOT EXISTS rail               TEXT         NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS connector_id       TEXT         NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS sent_payload_hash  TEXT         NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS sent_at            TIMESTAMPTZ  NULL,
    ADD COLUMN IF NOT EXISTS in_flight_since    TIMESTAMPTZ  NULL,
    ADD COLUMN IF NOT EXISTS provider_status    TEXT         NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS send_count         INTEGER      NOT NULL DEFAULT 0 CHECK (send_count >= 0),
    ADD COLUMN IF NOT EXISTS not_before         TIMESTAMPTZ  NULL,
    ADD COLUMN IF NOT EXISTS last_failure       TEXT         NOT NULL DEFAULT '';

ALTER TABLE payment_initiation_attempts DROP CONSTRAINT IF EXISTS payment_initiation_attempts_status_check;
ALTER TABLE payment_initiation_attempts ADD CONSTRAINT payment_initiation_attempts_status_check
    CHECK (status IN ('PREPARED', 'SUBMITTING', 'SUBMITTED', 'PENDING_UNKNOWN',
                      'REJECTED_BEFORE_SUBMISSION', 'CANCELLED', 'QUARANTINED'));

-- SUBMITTING always carries its start time; nothing else does.
ALTER TABLE payment_initiation_attempts DROP CONSTRAINT IF EXISTS attempt_in_flight_consistent;
ALTER TABLE payment_initiation_attempts ADD CONSTRAINT attempt_in_flight_consistent
    CHECK ((status = 'SUBMITTING') = (in_flight_since IS NOT NULL));

CREATE INDEX IF NOT EXISTS idx_attempts_in_flight ON payment_initiation_attempts (in_flight_since)
    WHERE status = 'SUBMITTING';

-- The guard: everything it enforced before, plus the rules that make a retry identical to the original.
CREATE OR REPLACE FUNCTION reject_attempt_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'payment_initiation_attempts rows are never deleted';
    END IF;

    IF OLD.status IN ('SUBMITTED', 'REJECTED_BEFORE_SUBMISSION', 'CANCELLED', 'QUARANTINED') THEN
        RAISE EXCEPTION 'attempt % is in terminal status % and cannot be modified', OLD.attempt_id, OLD.status;
    END IF;

    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id
        OR NEW.source_reference IS DISTINCT FROM OLD.source_reference
        OR NEW.authorization_fingerprint IS DISTINCT FROM OLD.authorization_fingerprint
        OR NEW.authorization_id IS DISTINCT FROM OLD.authorization_id
        OR NEW.authorization_source IS DISTINCT FROM OLD.authorization_source
        OR NEW.payer_account_ref IS DISTINCT FROM OLD.payer_account_ref
        OR NEW.payee_ref IS DISTINCT FROM OLD.payee_ref
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.execution_date IS DISTINCT FROM OLD.execution_date
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'attempt % is prepared; its authorized fields can never change', OLD.attempt_id;
    END IF;

    -- The provider context may be set once (at prepare, or pinned at the first send) and never changed:
    -- the same attempt always goes to the same provider, on the same rail, through the same connector,
    -- under the same certified profile version, carrying the same payload.
    IF (OLD.provider_key <> '' AND NEW.provider_key IS DISTINCT FROM OLD.provider_key)
        OR (OLD.rail <> '' AND NEW.rail IS DISTINCT FROM OLD.rail)
        OR (OLD.connector_id <> '' AND NEW.connector_id IS DISTINCT FROM OLD.connector_id)
        OR (OLD.profile_version IS NOT NULL AND NEW.profile_version IS DISTINCT FROM OLD.profile_version)
        OR (OLD.sent_payload_hash <> '' AND NEW.sent_payload_hash IS DISTINCT FROM OLD.sent_payload_hash)
    THEN
        RAISE EXCEPTION 'attempt % provider context is pinned; a retry is sent exactly as the original', OLD.attempt_id;
    END IF;

    IF NEW.send_count < OLD.send_count THEN
        RAISE EXCEPTION 'attempt % send_count can only increase', OLD.attempt_id;
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        -- a request is only ever put in flight from a state where sending is allowed
        IF NEW.status = 'SUBMITTING' AND OLD.status NOT IN ('PREPARED', 'PENDING_UNKNOWN') THEN
            RAISE EXCEPTION 'attempt % cannot start sending from %', OLD.attempt_id, OLD.status;
        END IF;
        -- an in-flight request is resolved by what the provider said, not by an operator action
        IF OLD.status = 'SUBMITTING' AND NEW.status IN ('CANCELLED', 'QUARANTINED') THEN
            RAISE EXCEPTION 'attempt % is in flight and cannot be %', OLD.attempt_id, NEW.status;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ── provider profiles (platform reference data) ─────────────────────────────
CREATE TABLE IF NOT EXISTS provider_profiles (
    provider_key  VARCHAR(80)  NOT NULL CHECK (provider_key ~ '^[a-z0-9][a-z0-9._-]{2,79}$'),
    version       INTEGER      NOT NULL CHECK (version >= 1),
    kind          VARCHAR(20)  NOT NULL CHECK (kind IN ('BANK', 'TAX', 'EINVOICE')),
    status        VARCHAR(20)  NOT NULL DEFAULT 'DRAFT' CHECK (status IN ('DRAFT', 'CERTIFIED', 'DEPRECATED', 'WITHDRAWN')),
    spec          JSONB        NOT NULL,
    spec_hash     VARCHAR(64)  NOT NULL CHECK (spec_hash ~ '^[0-9a-f]{64}$'),
    created_by    VARCHAR(255) NOT NULL,
    certified_by  VARCHAR(255),
    certified_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (provider_key, version),
    CONSTRAINT provider_profile_certified_by_other CHECK (
        status IN ('DRAFT', 'WITHDRAWN') OR (certified_by IS NOT NULL AND certified_at IS NOT NULL AND certified_by <> created_by))
);

CREATE OR REPLACE FUNCTION provider_profile_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'provider profiles are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.provider_key IS DISTINCT FROM OLD.provider_key OR NEW.version IS DISTINCT FROM OLD.version
       OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.kind IS DISTINCT FROM OLD.kind THEN
        RAISE EXCEPTION 'provider profile identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'DRAFT' AND (NEW.spec IS DISTINCT FROM OLD.spec OR NEW.spec_hash IS DISTINCT FROM OLD.spec_hash) THEN
        RAISE EXCEPTION 'a certified provider profile is immutable; publish a new version' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> NEW.status AND NOT (
        (OLD.status = 'DRAFT' AND NEW.status IN ('CERTIFIED', 'WITHDRAWN'))
        OR (OLD.status = 'CERTIFIED' AND NEW.status IN ('DEPRECATED', 'WITHDRAWN'))
        OR (OLD.status = 'DEPRECATED' AND NEW.status = 'WITHDRAWN')) THEN
        RAISE EXCEPTION 'provider profile status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_provider_profile_guard ON provider_profiles;
CREATE TRIGGER trg_provider_profile_guard BEFORE UPDATE OR DELETE ON provider_profiles
    FOR EACH ROW EXECUTE FUNCTION provider_profile_guard();

-- ── transport log (append-only evidence of every provider call) ─────────────
CREATE TABLE IF NOT EXISTS provider_transport_log (
    log_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID         NOT NULL,
    attempt_id      UUID         NOT NULL REFERENCES payment_initiation_attempts (attempt_id),
    provider_key    TEXT         NOT NULL,
    profile_version INTEGER      NOT NULL,
    operation       VARCHAR(10)  NOT NULL CHECK (operation IN ('SUBMIT', 'QUERY')),
    disposition     VARCHAR(10)  NOT NULL CHECK (disposition IN ('NOT_SENT', 'ANSWERED', 'UNKNOWN')),
    class           VARCHAR(20)  NOT NULL DEFAULT '',
    http_status     INTEGER      NOT NULL DEFAULT 0,
    request_hash    TEXT         NOT NULL DEFAULT '',
    response_hash   TEXT         NOT NULL DEFAULT '',
    provider_ref    TEXT         NOT NULL DEFAULT '',
    error_code      TEXT         NOT NULL DEFAULT '',
    duration_ms     INTEGER      NOT NULL DEFAULT 0,
    retry_after_s   INTEGER      NOT NULL DEFAULT 0,
    actor           TEXT         NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_transport_log_attempt ON provider_transport_log (attempt_id, created_at);
CREATE INDEX IF NOT EXISTS idx_transport_log_provider ON provider_transport_log (provider_key, created_at DESC);
DROP TRIGGER IF EXISTS trg_transport_log_immutable ON provider_transport_log;
CREATE TRIGGER trg_transport_log_immutable BEFORE UPDATE OR DELETE ON provider_transport_log
    FOR EACH ROW EXECUTE FUNCTION reject_evidence_mutation();

-- ── row-level security ──────────────────────────────────────────────────────
ALTER TABLE provider_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_profiles FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS provider_profiles_read ON provider_profiles;
CREATE POLICY provider_profiles_read ON provider_profiles FOR SELECT USING (true);
DROP POLICY IF EXISTS provider_profiles_platform_write ON provider_profiles;
CREATE POLICY provider_profiles_platform_write ON provider_profiles FOR INSERT
    WITH CHECK (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001');
DROP POLICY IF EXISTS provider_profiles_platform_update ON provider_profiles;
CREATE POLICY provider_profiles_platform_update ON provider_profiles FOR UPDATE
    USING (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001')
    WITH CHECK (current_setting('app.tenant_id', true) = '00000000-0000-0000-0000-00000000f001');

ALTER TABLE provider_transport_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_transport_log FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON provider_transport_log;
CREATE POLICY tenant_isolation ON provider_transport_log FOR ALL
    USING (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''));

-- The stale in-flight sweeper finds SUBMITTING attempts across tenants. It names itself instead of
-- running unscoped: the flag admits it to SELECT and UPDATE attempts and to nothing else; each attempt is
-- then resolved in a tenant-scoped transaction.
DROP POLICY IF EXISTS attempts_sweeper_read ON payment_initiation_attempts;
CREATE POLICY attempts_sweeper_read ON payment_initiation_attempts FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.provider_sweeper', true), ''), 'false') = 'true');
