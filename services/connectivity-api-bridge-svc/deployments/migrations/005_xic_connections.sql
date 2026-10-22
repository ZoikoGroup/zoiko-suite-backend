-- 005 ZS-SVC-T-001 XIC-02: Connection Lifecycle & Credential Binding.
--
-- A connection binds ONE tenant and environment to an approved connector, pinned to a published contract
-- version, with credential REFERENCES (never secret bytes) per authentication profile.
--
-- What the database guarantees on its own:
--   * every table is tenant-isolated by row-level security (FORCEd); the expiry worker reaches across tenants only
--     through an explicit worker flag, and may only read;
--   * tenant, environment and connector can never change; the contract pin changes only inside an approved
--     migration (session flag) and every pin change is a recorded request approved by someone else;
--   * status moves only along DRAFT > AUTHORIZING > VALIDATING > ACTIVE <> DEGRADED > SUSPENDED > REVOKED, REVOKED is
--     terminal and nothing is ever deleted;
--   * ACTIVE needs a PASSED validation of exactly this configuration, an activator who is not the creator and, in
--     PRODUCTION, a PASSED, unexpired certification bound to exactly this contract, mappings and auth profiles;
--   * a credential reference belongs to at most one live binding in the whole platform (a reference seen in two
--     tenants is a cross-tenant defect, not a sharing arrangement);
--   * authorization state is single-use; checks, certifications and events are append-only.

CREATE TABLE IF NOT EXISTS xic_connections (
    connection_id        UUID         PRIMARY KEY,
    tenant_id            VARCHAR(64)  NOT NULL,
    environment          VARCHAR(12)  NOT NULL,
    connector_id         VARCHAR(63)  NOT NULL REFERENCES xic_connectors(connector_id),
    contract_version_id   UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    status               VARCHAR(12)  NOT NULL DEFAULT 'DRAFT',
    status_reason        VARCHAR(255) NOT NULL DEFAULT '',
    region               VARCHAR(32)  NOT NULL,
    endpoints            JSONB        NOT NULL DEFAULT '{}',
    provider_account_hash VARCHAR(64) NOT NULL,
    external_org_hash    VARCHAR(64)  NOT NULL DEFAULT '',
    bridge_id            VARCHAR(64),
    config_fingerprint   VARCHAR(64)  NOT NULL DEFAULT '',
    capability_snapshot  JSONB,
    created_by           VARCHAR(255) NOT NULL,
    consent_actor        VARCHAR(255) NOT NULL DEFAULT '',
    activated_by         VARCHAR(255) NOT NULL DEFAULT '',
    activated_at         TIMESTAMPTZ,
    suspended_at         TIMESTAMPTZ,
    revoked_at           TIMESTAMPTZ,
    version              BIGINT       NOT NULL DEFAULT 1,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_conn_env_valid CHECK (environment IN ('PRODUCTION', 'STAGING', 'SANDBOX')),
    CONSTRAINT xic_conn_status_valid CHECK (status IN
        ('DRAFT', 'AUTHORIZING', 'VALIDATING', 'ACTIVE', 'DEGRADED', 'SUSPENDED', 'REVOKED')),
    CONSTRAINT xic_conn_hash_valid CHECK (provider_account_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_conn_version_positive CHECK (version >= 1),
    CONSTRAINT xic_conn_region_valid CHECK (region ~ '^[a-z][a-z0-9-]{1,31}$'),
    CONSTRAINT xic_conn_endpoints_object CHECK (jsonb_typeof(endpoints) = 'object'),
    CONSTRAINT xic_conn_live_is_activated CHECK (
        status NOT IN ('ACTIVE', 'DEGRADED') OR (activated_by <> '' AND activated_by <> created_by AND activated_at IS NOT NULL
                                                  AND config_fingerprint ~ '^[0-9a-f]{64}$')),
    CONSTRAINT xic_conn_revoked_dated CHECK (status <> 'REVOKED' OR revoked_at IS NOT NULL),
    CONSTRAINT xic_conn_reasoned CHECK (status NOT IN ('SUSPENDED', 'REVOKED', 'DEGRADED') OR status_reason <> '')
);
-- one live connection per (tenant, environment, connector, provider account): exclusivity of the external account
CREATE UNIQUE INDEX IF NOT EXISTS xic_conn_one_per_account ON xic_connections
    (tenant_id, environment, connector_id, provider_account_hash) WHERE status <> 'REVOKED';
CREATE INDEX IF NOT EXISTS xic_conn_tenant ON xic_connections (tenant_id, status);
CREATE INDEX IF NOT EXISTS xic_conn_pin ON xic_connections (contract_version_id) WHERE status <> 'REVOKED';

CREATE TABLE IF NOT EXISTS xic_credential_bindings (
    binding_id          UUID         PRIMARY KEY,
    connection_id       UUID         NOT NULL REFERENCES xic_connections(connection_id),
    tenant_id           VARCHAR(64)  NOT NULL,
    auth_profile        VARCHAR(63)  NOT NULL,
    current_ref         VARCHAR(64)  NOT NULL,
    previous_ref        VARCHAR(64),
    previous_grace_until TIMESTAMPTZ,
    granted_scopes      TEXT[]       NOT NULL DEFAULT '{}',
    scope_fingerprint   VARCHAR(64)  NOT NULL,
    excess_scope_ack_by VARCHAR(255) NOT NULL DEFAULT '',
    status              VARCHAR(10)  NOT NULL DEFAULT 'OK',
    expires_at          TIMESTAMPTZ,
    rotation_due_at     TIMESTAMPTZ,
    bound_by            VARCHAR(255) NOT NULL,
    bound_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_bind_unique UNIQUE (connection_id, auth_profile),
    CONSTRAINT xic_bind_status_valid CHECK (status IN ('OK', 'EXPIRING', 'EXPIRED', 'REVOKED')),
    CONSTRAINT xic_bind_ref_valid CHECK (current_ref ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    CONSTRAINT xic_bind_prev_valid CHECK (previous_ref IS NULL OR (previous_ref ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                                          AND previous_ref <> current_ref AND previous_grace_until IS NOT NULL)),
    CONSTRAINT xic_bind_fp_valid CHECK (scope_fingerprint ~ '^[0-9a-f]{64}$')
);
-- a credential reference is bound at most once across the platform
CREATE UNIQUE INDEX IF NOT EXISTS xic_bind_ref_exclusive ON xic_credential_bindings (lower(current_ref)) WHERE status <> 'REVOKED';

CREATE TABLE IF NOT EXISTS xic_connection_events (
    ord           BIGINT       GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    connection_id UUID         NOT NULL REFERENCES xic_connections(connection_id),
    tenant_id     VARCHAR(64)  NOT NULL,
    kind          VARCHAR(40)  NOT NULL,
    from_status   VARCHAR(12)  NOT NULL DEFAULT '',
    to_status     VARCHAR(12)  NOT NULL DEFAULT '',
    actor         VARCHAR(255) NOT NULL,
    reason        VARCHAR(255) NOT NULL DEFAULT '',
    at            TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS xic_conn_events_conn ON xic_connection_events (connection_id, ord);

CREATE TABLE IF NOT EXISTS xic_connection_checks (
    check_id           UUID         PRIMARY KEY,
    ord                BIGINT       GENERATED ALWAYS AS IDENTITY,
    connection_id      UUID         NOT NULL REFERENCES xic_connections(connection_id),
    tenant_id          VARCHAR(64)  NOT NULL,
    kind               VARCHAR(12)  NOT NULL,
    outcome            VARCHAR(8)   NOT NULL,
    config_fingerprint VARCHAR(64)  NOT NULL,
    issues             JSONB        NOT NULL DEFAULT '[]',
    detail             JSONB,
    run_by             VARCHAR(255) NOT NULL,
    at                 TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_check_kind_valid CHECK (kind IN ('VALIDATION', 'PROBE')),
    CONSTRAINT xic_check_outcome_valid CHECK (outcome IN ('PASSED', 'FAILED')),
    CONSTRAINT xic_check_pass_clean CHECK (outcome <> 'PASSED' OR jsonb_array_length(issues) = 0)
);
CREATE INDEX IF NOT EXISTS xic_checks_conn ON xic_connection_checks (connection_id, kind, ord);

CREATE TABLE IF NOT EXISTS xic_certifications (
    certification_id    UUID         PRIMARY KEY,
    ord                 BIGINT       GENERATED ALWAYS AS IDENTITY,
    connection_id       UUID         NOT NULL REFERENCES xic_connections(connection_id),
    tenant_id           VARCHAR(64)  NOT NULL,
    environment         VARCHAR(12)  NOT NULL,
    contract_version_id UUID         NOT NULL REFERENCES xic_contract_versions(contract_version_id),
    scope_fingerprint   VARCHAR(64)  NOT NULL,
    test_pack_version   VARCHAR(64)  NOT NULL,
    passed_tests        TEXT[]       NOT NULL DEFAULT '{}',
    outcome             VARCHAR(8)   NOT NULL,
    certified_by        VARCHAR(255) NOT NULL,
    at                  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    expires_at          TIMESTAMPTZ  NOT NULL,
    CONSTRAINT xic_cert_outcome_valid CHECK (outcome IN ('PASSED', 'FAILED')),
    CONSTRAINT xic_cert_fp_valid CHECK (scope_fingerprint ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_cert_expiry CHECK (expires_at > at)
);
CREATE INDEX IF NOT EXISTS xic_certs_conn ON xic_certifications (connection_id, ord);

CREATE TABLE IF NOT EXISTS xic_authorization_sessions (
    session_id    UUID         PRIMARY KEY,
    connection_id UUID         NOT NULL REFERENCES xic_connections(connection_id),
    tenant_id     VARCHAR(64)  NOT NULL,
    state_hash    VARCHAR(64)  NOT NULL,
    redirect_uri  VARCHAR(512) NOT NULL,
    expires_at    TIMESTAMPTZ  NOT NULL,
    consumed_at   TIMESTAMPTZ,
    created_by    VARCHAR(255) NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_session_hash_valid CHECK (state_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT xic_session_expiry CHECK (expires_at > created_at)
);
CREATE INDEX IF NOT EXISTS xic_sessions_conn ON xic_authorization_sessions (connection_id, created_at);

-- governed requests: contract migrations and break-glass repairs
CREATE TABLE IF NOT EXISTS xic_connection_requests (
    request_id    UUID         PRIMARY KEY,
    connection_id UUID         NOT NULL REFERENCES xic_connections(connection_id),
    tenant_id     VARCHAR(64)  NOT NULL,
    kind          VARCHAR(12)  NOT NULL,
    payload       JSONB        NOT NULL,
    reason        VARCHAR(255) NOT NULL,
    status        VARCHAR(10)  NOT NULL DEFAULT 'PENDING',
    requested_by  VARCHAR(255) NOT NULL,
    decided_by    VARCHAR(255),
    decided_at    TIMESTAMPTZ,
    effective_at  TIMESTAMPTZ,
    expires_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT xic_req_kind_valid CHECK (kind IN ('MIGRATION', 'BREAK_GLASS')),
    CONSTRAINT xic_req_status_valid CHECK (status IN ('PENDING', 'APPROVED', 'REJECTED', 'EXPIRED', 'USED')),
    CONSTRAINT xic_req_distinct_people CHECK (decided_by IS NULL OR decided_by <> requested_by),
    CONSTRAINT xic_req_decided_has_actor CHECK (status = 'PENDING' OR decided_by IS NOT NULL OR status = 'EXPIRED'),
    CONSTRAINT xic_req_reasoned CHECK (btrim(reason) <> '')
);
CREATE INDEX IF NOT EXISTS xic_requests_conn ON xic_connection_requests (connection_id, kind, created_at);

CREATE TABLE IF NOT EXISTS xic_conn_idempotency (
    tenant_id    VARCHAR(64)  NOT NULL,
    scope        VARCHAR(24)  NOT NULL,
    idem_key     VARCHAR(128) NOT NULL,
    request_hash VARCHAR(64)  NOT NULL,
    resource_id  VARCHAR(80)  NOT NULL,
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, scope, idem_key)
);

-- ── append-only ─────────────────────────────────────────────────────────────

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['xic_connection_events', 'xic_connection_checks', 'xic_certifications', 'xic_conn_idempotency']
    LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS %I ON %I', t || '_no_change', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION xic_append_only()', t || '_no_change', t);
    END LOOP;
END $$;

-- ── connection state machine ────────────────────────────────────────────────

CREATE OR REPLACE FUNCTION xic_conn_guard() RETURNS trigger AS $$
DECLARE
    v_ok INT;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a connection is never deleted; revoke it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.connection_id <> OLD.connection_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.environment <> OLD.environment
       OR NEW.connector_id <> OLD.connector_id OR NEW.created_by <> OLD.created_by OR NEW.created_at <> OLD.created_at
       OR NEW.provider_account_hash <> OLD.provider_account_hash THEN
        RAISE EXCEPTION 'tenant, environment, connector and provider account of a connection are immutable'
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'connection version must advance by exactly one' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'REVOKED' THEN
        RAISE EXCEPTION 'a revoked connection is terminal' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    -- the contract pin moves only inside an approved migration: the flag says the application is applying one, and
    -- the database itself requires an approved (or already-used) migration request naming exactly this target
    IF NEW.contract_version_id <> OLD.contract_version_id THEN
        IF COALESCE(current_setting('app.xic_migration', true), '') <> 'true' OR NOT EXISTS (
            SELECT 1 FROM xic_connection_requests q
             WHERE q.connection_id = NEW.connection_id AND q.kind = 'MIGRATION' AND q.status IN ('APPROVED', 'USED')
               AND q.payload->>'to_contract_version_id' = NEW.contract_version_id::text
               AND q.payload->>'from_contract_version_id' = OLD.contract_version_id::text) THEN
            RAISE EXCEPTION 'a connection stays pinned until a governed migration moves it' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    IF OLD.status <> NEW.status AND NOT (
           (OLD.status = 'DRAFT'       AND NEW.status IN ('AUTHORIZING', 'REVOKED'))
        OR (OLD.status = 'AUTHORIZING' AND NEW.status IN ('VALIDATING', 'REVOKED', 'DRAFT'))
        OR (OLD.status = 'VALIDATING'  AND NEW.status IN ('ACTIVE', 'AUTHORIZING', 'REVOKED'))
        OR (OLD.status = 'ACTIVE'      AND NEW.status IN ('DEGRADED', 'SUSPENDED', 'REVOKED'))
        OR (OLD.status = 'DEGRADED'    AND NEW.status IN ('ACTIVE', 'SUSPENDED', 'REVOKED'))
        OR (OLD.status = 'SUSPENDED'   AND NEW.status IN ('AUTHORIZING', 'VALIDATING', 'REVOKED'))) THEN
        RAISE EXCEPTION 'connection status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status = 'VALIDATING' AND OLD.status = 'AUTHORIZING' THEN
        -- entering validation needs a PASSED validation of exactly this configuration
        SELECT 1 INTO v_ok FROM xic_connection_checks c
         WHERE c.connection_id = NEW.connection_id AND c.kind = 'VALIDATION'
           AND c.ord = (SELECT max(ord) FROM xic_connection_checks WHERE connection_id = NEW.connection_id AND kind = 'VALIDATION')
           AND c.outcome = 'PASSED' AND c.config_fingerprint = NEW.config_fingerprint;
        IF v_ok IS NULL THEN
            RAISE EXCEPTION 'validation needs a passed validation of exactly this configuration' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    IF NEW.status = 'ACTIVE' AND OLD.status = 'VALIDATING' THEN
        -- a PASSED validation of exactly this configuration
        SELECT 1 INTO v_ok FROM xic_connection_checks c
         WHERE c.connection_id = NEW.connection_id AND c.kind = 'VALIDATION'
           AND c.ord = (SELECT max(ord) FROM xic_connection_checks WHERE connection_id = NEW.connection_id AND kind = 'VALIDATION')
           AND c.outcome = 'PASSED' AND c.config_fingerprint = NEW.config_fingerprint;
        IF v_ok IS NULL THEN
            RAISE EXCEPTION 'activation needs a passed validation of exactly this configuration' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.environment = 'PRODUCTION' THEN
            v_ok := NULL;
            SELECT 1 INTO v_ok FROM xic_certifications z
             WHERE z.connection_id = NEW.connection_id
               AND z.ord = (SELECT max(ord) FROM xic_certifications WHERE connection_id = NEW.connection_id)
               AND z.outcome = 'PASSED' AND z.expires_at > now() AND z.contract_version_id = NEW.contract_version_id
               AND z.environment = NEW.environment;
            IF v_ok IS NULL THEN
                RAISE EXCEPTION 'a production connection is certified for this contract before it is activated' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_conn_guard_trg ON xic_connections;
CREATE TRIGGER xic_conn_guard_trg BEFORE UPDATE OR DELETE ON xic_connections FOR EACH ROW EXECUTE FUNCTION xic_conn_guard();

-- Creating a connection pins it to a PUBLISHED contract of the same connector.
CREATE OR REPLACE FUNCTION xic_conn_insert_guard() RETURNS trigger AS $$
DECLARE
    v_connector TEXT; v_status TEXT;
BEGIN
    SELECT connector_id, status INTO v_connector, v_status FROM xic_contract_versions WHERE contract_version_id = NEW.contract_version_id;
    IF v_connector IS DISTINCT FROM NEW.connector_id THEN
        RAISE EXCEPTION 'the contract version does not belong to this connector' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF v_status <> 'PUBLISHED' THEN
        RAISE EXCEPTION 'a new connection pins a PUBLISHED contract version' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.status <> 'DRAFT' THEN
        RAISE EXCEPTION 'a connection is created as DRAFT' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_conn_insert_guard_trg ON xic_connections;
CREATE TRIGGER xic_conn_insert_guard_trg BEFORE INSERT ON xic_connections FOR EACH ROW EXECUTE FUNCTION xic_conn_insert_guard();

CREATE OR REPLACE FUNCTION xic_bind_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'a credential binding is never deleted; revoke it' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.connection_id <> OLD.connection_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.auth_profile <> OLD.auth_profile THEN
        RAISE EXCEPTION 'binding identity is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status = 'REVOKED' AND NEW.status <> 'REVOKED' THEN
        RAISE EXCEPTION 'a revoked binding stays revoked' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_bind_guard_trg ON xic_credential_bindings;
CREATE TRIGGER xic_bind_guard_trg BEFORE UPDATE OR DELETE ON xic_credential_bindings FOR EACH ROW EXECUTE FUNCTION xic_bind_guard();

CREATE OR REPLACE FUNCTION xic_session_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'authorization sessions are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.state_hash <> OLD.state_hash OR NEW.redirect_uri <> OLD.redirect_uri OR NEW.expires_at <> OLD.expires_at
       OR NEW.connection_id <> OLD.connection_id OR NEW.tenant_id <> OLD.tenant_id THEN
        RAISE EXCEPTION 'an authorization session is immutable' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.consumed_at IS NOT NULL THEN
        RAISE EXCEPTION 'an authorization state is single-use' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.consumed_at IS NOT NULL AND NEW.consumed_at > OLD.expires_at THEN
        RAISE EXCEPTION 'an expired authorization state cannot be consumed' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_session_guard_trg ON xic_authorization_sessions;
CREATE TRIGGER xic_session_guard_trg BEFORE UPDATE OR DELETE ON xic_authorization_sessions FOR EACH ROW EXECUTE FUNCTION xic_session_guard();

CREATE OR REPLACE FUNCTION xic_req_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'requests are never deleted' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.request_id <> OLD.request_id OR NEW.connection_id <> OLD.connection_id OR NEW.kind <> OLD.kind
       OR NEW.payload <> OLD.payload OR NEW.requested_by <> OLD.requested_by OR NEW.reason <> OLD.reason THEN
        RAISE EXCEPTION 'a request is immutable apart from its decision' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'PENDING' AND NOT (OLD.status = 'APPROVED' AND NEW.status IN ('USED', 'EXPIRED')) AND NEW.status <> OLD.status THEN
        RAISE EXCEPTION 'a decided request does not change again' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS xic_req_guard_trg ON xic_connection_requests;
CREATE TRIGGER xic_req_guard_trg BEFORE UPDATE OR DELETE ON xic_connection_requests FOR EACH ROW EXECUTE FUNCTION xic_req_guard();

-- ── row-level security ──────────────────────────────────────────────────────
-- Tenant tables: the tenant reads and writes its own rows. The expiry worker and the cross-tenant resolver read
-- across tenants only with app.xic_worker, and only SELECT.

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['xic_connections', 'xic_credential_bindings', 'xic_connection_events', 'xic_connection_checks',
                             'xic_certifications', 'xic_authorization_sessions', 'xic_connection_requests', 'xic_conn_idempotency']
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
