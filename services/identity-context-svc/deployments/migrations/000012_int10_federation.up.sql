-- Migration: 000012_int10_federation.up.sql
--
-- INT-10 Identity Provider / Provisioning Adapter, part 1: tenant federation connections, the signing
-- keys they trust, the external-subject links they create, and the evidence of every federated login.
--
-- What this service owns here (spec 4.10): the tenant federation connection, its issuer / metadata /
-- profile version, the external-subject link evidence, and sync status. What it does NOT own: the human
-- or legal identity behind a subject, the IdP's internals, privileged role authority or the SoD
-- decision. Roles are never written here; they are requested from access-control-svc (part 2).
--
-- Every table is tenant-scoped with FORCE row level security, like the rest of this service. No code
-- path on the login route reads across tenants: the tenant is part of the request and the connection is
-- found inside it.

-- ---------------------------------------------------------------------------
-- 1. Connections and their immutable, maker-checker versions
-- ---------------------------------------------------------------------------
--
-- A connection is the named relationship with one tenant IdP. Its trust configuration lives in
-- versions. A version is frozen when it is created (a trigger below), is approved by someone other than
-- its author against the fingerprint the approver saw, and only then becomes the version login uses.
-- Changing an ACTIVE connection never interrupts it: the new version waits for approval while the
-- approved one keeps serving.

CREATE TABLE federation_connections (
    connection_id   VARCHAR(255) PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    name            VARCHAR(200) NOT NULL,
    protocol        VARCHAR(20)  NOT NULL DEFAULT 'OIDC',
    status          VARCHAR(20)  NOT NULL DEFAULT 'PENDING_APPROVAL',
    status_reason   TEXT         NOT NULL DEFAULT '',
    -- The approved version login uses. NULL until the first approval.
    active_version  INT,
    latest_version  INT          NOT NULL DEFAULT 1,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    CONSTRAINT federation_connections_protocol_check CHECK (protocol = 'OIDC'),
    CONSTRAINT federation_connections_status_check CHECK (status IN
        ('PENDING_APPROVAL', 'ACTIVE', 'DEGRADED', 'SUSPENDED', 'REVOKED')),
    -- A connection that can authenticate must have an approved version behind it.
    CONSTRAINT federation_connections_active_has_version CHECK (
        status IN ('PENDING_APPROVAL', 'REVOKED') OR active_version IS NOT NULL),
    UNIQUE (tenant_id, name)
);

CREATE TABLE federation_connection_versions (
    connection_id   VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    tenant_id       VARCHAR(255) NOT NULL,
    version         INT          NOT NULL,
    status          VARCHAR(20)  NOT NULL DEFAULT 'PENDING_APPROVAL',

    issuer          TEXT         NOT NULL,
    audience        VARCHAR(255) NOT NULL,
    jwks_uri        TEXT         NOT NULL,
    allowed_algs    TEXT[]       NOT NULL,
    subject_claim   VARCHAR(64)  NOT NULL DEFAULT 'sub',
    -- The claim an IdP sends the address in. Informational only: it is never an identity binding.
    email_claim     VARCHAR(64)  NOT NULL DEFAULT 'email',
    -- amr / acr values the tenant has approved as proof of a second factor. Empty means a federated
    -- login is never recorded as MFA_VERIFIED, whatever the IdP claims.
    mfa_amr_values  TEXT[]       NOT NULL DEFAULT '{}',
    mfa_acr_values  TEXT[]       NOT NULL DEFAULT '{}',
    nonce_required  BOOLEAN      NOT NULL DEFAULT TRUE,
    max_token_age_seconds INT    NOT NULL DEFAULT 300,
    clock_skew_seconds    INT    NOT NULL DEFAULT 60,
    -- IdP outage behaviour, per tenant (spec NFR). FAIL_CLOSED: no JWKS, no login. CACHED_KEYS: keep
    -- verifying against the last good key set, but only for key_grace_seconds after it was fetched.
    -- There is deliberately no value that skips verification.
    outage_behavior       VARCHAR(20) NOT NULL DEFAULT 'FAIL_CLOSED',
    key_grace_seconds     INT    NOT NULL DEFAULT 0,
    jit_enabled     BOOLEAN      NOT NULL DEFAULT FALSE,

    fingerprint     VARCHAR(64)  NOT NULL,
    created_by      VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    decided_by      VARCHAR(255),
    decided_at      TIMESTAMP WITH TIME ZONE,
    decision_reason TEXT         NOT NULL DEFAULT '',
    approved_fingerprint VARCHAR(64),

    PRIMARY KEY (connection_id, version),
    CONSTRAINT federation_versions_status_check CHECK (status IN
        ('PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'SUPERSEDED')),
    CONSTRAINT federation_versions_outage_check CHECK (outage_behavior IN ('FAIL_CLOSED', 'CACHED_KEYS')),
    CONSTRAINT federation_versions_grace_check CHECK (
        (outage_behavior = 'FAIL_CLOSED' AND key_grace_seconds = 0)
        OR (outage_behavior = 'CACHED_KEYS' AND key_grace_seconds BETWEEN 1 AND 86400)),
    CONSTRAINT federation_versions_skew_check CHECK (clock_skew_seconds BETWEEN 0 AND 120),
    CONSTRAINT federation_versions_age_check CHECK (max_token_age_seconds BETWEEN 30 AND 3600),
    -- Maker-checker, in the data as well as the service: the author cannot decide their own version,
    -- and an approval records exactly the fingerprint that was approved.
    CONSTRAINT federation_versions_maker_checker CHECK (decided_by IS NULL OR decided_by <> created_by),
    CONSTRAINT federation_versions_approval_fingerprint CHECK (
        status <> 'APPROVED' OR approved_fingerprint = fingerprint)
);

CREATE INDEX idx_federation_versions_tenant ON federation_connection_versions (tenant_id, connection_id);

-- A version's trust configuration never changes after it is written. Only its decision may be recorded,
-- once, and only by moving out of PENDING_APPROVAL (or APPROVED -> SUPERSEDED when a later one wins).
CREATE OR REPLACE FUNCTION federation_version_guard() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'federation versions are append-only' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.connection_id <> OLD.connection_id OR NEW.tenant_id <> OLD.tenant_id OR NEW.version <> OLD.version
       OR NEW.issuer <> OLD.issuer OR NEW.audience <> OLD.audience OR NEW.jwks_uri <> OLD.jwks_uri
       OR NEW.allowed_algs <> OLD.allowed_algs OR NEW.subject_claim <> OLD.subject_claim
       OR NEW.email_claim <> OLD.email_claim OR NEW.mfa_amr_values <> OLD.mfa_amr_values
       OR NEW.mfa_acr_values <> OLD.mfa_acr_values OR NEW.nonce_required <> OLD.nonce_required
       OR NEW.max_token_age_seconds <> OLD.max_token_age_seconds
       OR NEW.clock_skew_seconds <> OLD.clock_skew_seconds OR NEW.outage_behavior <> OLD.outage_behavior
       OR NEW.key_grace_seconds <> OLD.key_grace_seconds OR NEW.jit_enabled <> OLD.jit_enabled
       OR NEW.fingerprint <> OLD.fingerprint OR NEW.created_by <> OLD.created_by
       OR NEW.created_at <> OLD.created_at THEN
        RAISE EXCEPTION 'a federation version is immutable once written' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'PENDING_APPROVAL' AND NEW.status IS DISTINCT FROM OLD.status
       AND NOT (OLD.status = 'APPROVED' AND NEW.status = 'SUPERSEDED') THEN
        RAISE EXCEPTION 'a decided federation version cannot change status' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF OLD.status <> 'PENDING_APPROVAL' AND (NEW.decided_by IS DISTINCT FROM OLD.decided_by
       OR NEW.decided_at IS DISTINCT FROM OLD.decided_at
       OR NEW.approved_fingerprint IS DISTINCT FROM OLD.approved_fingerprint
       OR NEW.decision_reason IS DISTINCT FROM OLD.decision_reason) THEN
        RAISE EXCEPTION 'a federation decision is recorded once' USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER federation_version_guard BEFORE UPDATE OR DELETE ON federation_connection_versions
    FOR EACH ROW EXECUTE FUNCTION federation_version_guard();

-- Connection lifecycle and trust changes are an append-only trail.
CREATE TABLE federation_connection_events (
    event_id        VARCHAR(255) PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    connection_id   VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    event_type      VARCHAR(40)  NOT NULL,
    version         INT,
    actor_id        VARCHAR(255) NOT NULL,
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    detail          JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_federation_events_conn ON federation_connection_events (connection_id, event_id);

-- ---------------------------------------------------------------------------
-- 2. Signing keys: the last key set fetched from each connection's approved JWKS URI
-- ---------------------------------------------------------------------------
--
-- One row per connection. jwks_uri is stored beside the keys so that a rotated URI (a new approved
-- version) can never be served from keys fetched from the old one.

CREATE TABLE federation_key_sets (
    connection_id   VARCHAR(255) PRIMARY KEY REFERENCES federation_connections(connection_id),
    tenant_id       VARCHAR(255) NOT NULL,
    jwks_uri        TEXT         NOT NULL,
    jwks            JSONB        NOT NULL,
    key_count       INT          NOT NULL,
    fetched_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    last_attempt_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_error      TEXT         NOT NULL DEFAULT '',
    consecutive_failures INT     NOT NULL DEFAULT 0
);

-- ---------------------------------------------------------------------------
-- 3. External subject links
-- ---------------------------------------------------------------------------
--
-- An external identity is (tenant, issuer, stable subject). Never the email: two issuers may assert the
-- same address for two different people, and an address can be reassigned. The link points at the
-- principal that holds access here; it carries no role.

CREATE TABLE federation_links (
    link_id         VARCHAR(255) PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    connection_id   VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    issuer          TEXT         NOT NULL,
    subject         VARCHAR(255) NOT NULL,
    principal_id    VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    status          VARCHAR(20)  NOT NULL DEFAULT 'ACTIVE',
    link_source     VARCHAR(20)  NOT NULL,
    linked_by       VARCHAR(255) NOT NULL,
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    last_login_at   TIMESTAMP WITH TIME ZONE,
    deprovisioned_at TIMESTAMP WITH TIME ZONE,

    CONSTRAINT federation_links_status_check CHECK (status IN ('ACTIVE', 'SUSPENDED', 'DEPROVISIONED')),
    CONSTRAINT federation_links_source_check CHECK (link_source IN ('SCIM', 'JIT', 'ADMIN')),
    UNIQUE (tenant_id, issuer, subject),
    UNIQUE (tenant_id, issuer, principal_id)
);
CREATE INDEX idx_federation_links_principal ON federation_links (tenant_id, principal_id);

-- ---------------------------------------------------------------------------
-- 4. Login evidence and single-use ID tokens
-- ---------------------------------------------------------------------------
--
-- A row is written only for a login whose token signature VERIFIED against the tenant's approved keys
-- (a success, or a denial after authentication). Input that has not proven a signature writes nothing
-- durable: it is counted, logged and streamed, so an anonymous sender cannot grow this table.

CREATE TABLE federation_login_log (
    log_id          VARCHAR(255) PRIMARY KEY,
    tenant_id       VARCHAR(255) NOT NULL,
    connection_id   VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    config_version  INT          NOT NULL,
    issuer          TEXT         NOT NULL,
    subject         VARCHAR(255) NOT NULL,
    principal_id    VARCHAR(255),
    outcome         VARCHAR(20)  NOT NULL,
    reason          VARCHAR(60)  NOT NULL,
    signature_check VARCHAR(20)  NOT NULL DEFAULT 'VALID',
    key_id          VARCHAR(255) NOT NULL DEFAULT '',
    mfa_asserted    BOOLEAN      NOT NULL DEFAULT FALSE,
    degraded_keys   BOOLEAN      NOT NULL DEFAULT FALSE,
    correlation_id  VARCHAR(255) NOT NULL DEFAULT '',
    created_at      TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    CONSTRAINT federation_login_outcome_check CHECK (outcome IN ('SUCCEEDED', 'DENIED')),
    CONSTRAINT federation_login_signature_check CHECK (signature_check = 'VALID')
);
CREATE INDEX idx_federation_login_conn ON federation_login_log (connection_id, created_at DESC);

-- An ID token is exchanged for a session once. Written only after the signature verified.
CREATE TABLE federation_token_uses (
    connection_id   VARCHAR(255) NOT NULL REFERENCES federation_connections(connection_id),
    tenant_id       VARCHAR(255) NOT NULL,
    token_hash      VARCHAR(64)  NOT NULL,
    expires_at      TIMESTAMP WITH TIME ZONE NOT NULL,
    used_at         TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (connection_id, token_hash)
);
CREATE INDEX idx_federation_token_uses_expiry ON federation_token_uses (expires_at);

-- Evidence is append-only.
CREATE OR REPLACE FUNCTION federation_evidence_guard() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER federation_login_log_guard BEFORE UPDATE OR DELETE ON federation_login_log
    FOR EACH ROW EXECUTE FUNCTION federation_evidence_guard();
CREATE TRIGGER federation_connection_events_guard BEFORE UPDATE OR DELETE ON federation_connection_events
    FOR EACH ROW EXECUTE FUNCTION federation_evidence_guard();

-- ---------------------------------------------------------------------------
-- 5. Row level security: tenant isolation, FORCE
-- ---------------------------------------------------------------------------

ALTER TABLE federation_connections ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_connections FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_connections FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_connection_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_connection_versions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_connection_versions FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_connection_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_connection_events FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_connection_events FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_key_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_key_sets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_key_sets FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_links FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_links FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_login_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_login_log FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_login_log FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE federation_token_uses ENABLE ROW LEVEL SECURITY;
ALTER TABLE federation_token_uses FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON federation_token_uses FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
