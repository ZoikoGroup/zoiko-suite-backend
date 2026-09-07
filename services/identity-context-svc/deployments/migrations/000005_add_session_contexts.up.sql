-- Migration: 000005_add_session_contexts.up.sql
--
-- Gives SessionContext a durable home.
--
-- Resolve() has always built a SessionContext record and called it an
-- "append-only evidence obligation", but the only write was to Redis under
-- session:ctx:<id> with the session's own TTL. Evidence that expires is not
-- evidence: within the hour, there was no record anywhere on the platform that
-- a session had ever been issued, to whom, under what trust posture, or why it
-- was later invalidated.
--
-- The Redis copy stays. It serves GET /v1/context/session/{id} on the hot path
-- and is the reason that read does not touch Postgres. This table is the
-- durable copy behind it, written on the same call.
--
-- Deliberately NOT an outbox. An outbox decouples the evidence write from the
-- request so a Postgres outage cannot fail a resolution — that is the right
-- end state and it is tracked separately, because an outbox is an estate-wide
-- pattern and building one inside a single service is how four incompatible
-- outboxes get built. Until then a direct write that is logged-and-swallowed on
-- failure loses strictly less than the status quo, which lost everything after
-- the TTL regardless of whether anything was wrong.
--
-- ID columns are VARCHAR, matching 000001: session_context_id is a ULID, not a
-- valid Postgres UUID literal.
--
-- Migrations are run via golang-migrate CLI in CI/CD. Do NOT auto-run on
-- service startup.

CREATE TABLE session_contexts (
    session_context_id       VARCHAR(255) PRIMARY KEY,
    principal_id             VARCHAR(255) NOT NULL REFERENCES principals(principal_id),
    tenant_id                VARCHAR(255) NOT NULL,

    -- Nullable because a tenant-wide session names no entity. The resolver
    -- always supplies one today; the column does not force that to stay true.
    legal_entity_id          VARCHAR(255),
    correlation_id           VARCHAR(255) NOT NULL,

    -- The six-dimension outcome, frozen at issue time. Re-deriving it later
    -- would read today's risk signals against a session issued last week.
    trust_posture            VARCHAR(50)  NOT NULL,
    mfa_verified             BOOLEAN      NOT NULL,
    device_trust_score       INT          NOT NULL DEFAULT 0,
    adaptive_risk_score      INT          NOT NULL DEFAULT 0,

    -- UNAVAILABLE is a real, recorded answer, not a null. A null would be
    -- indistinguishable from "we forgot to write it"; the distinction matters
    -- because a session resolved with no risk signal is one whose posture was
    -- defaulted rather than measured.
    risk_signal_source       VARCHAR(50)  NOT NULL,

    -- Ties the record to the envelope actually issued, so a JWT presented in an
    -- incident can be traced back to the session that minted it.
    envelope_jwt_jti         VARCHAR(255) NOT NULL,

    issued_at                TIMESTAMP WITH TIME ZONE NOT NULL,
    expires_at               TIMESTAMP WITH TIME ZONE NOT NULL,

    -- Append-only. A row is never deleted and never rewritten except to set
    -- these two, exactly once, on invalidation.
    invalidated_at           TIMESTAMP WITH TIME ZONE,
    invalidation_reason      VARCHAR(50),

    data_residency_policy_id VARCHAR(255) NOT NULL DEFAULT '',
    source_service           VARCHAR(100) NOT NULL,
    schema_version           VARCHAR(20)  NOT NULL,
    created_at               TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),

    CONSTRAINT session_contexts_trust_posture_check
        CHECK (trust_posture IN ('STANDARD', 'ELEVATED', 'MFA_VERIFIED', 'HIGH_RISK', 'BLOCKED')),

    CONSTRAINT session_contexts_invalidation_reason_check
        CHECK (invalidation_reason IS NULL OR invalidation_reason IN
               ('LOGOUT', 'ADMIN_REVOKE', 'RISK_ESCALATION', 'DELEGATION_REVOKED')),

    -- A reason without a timestamp, or a timestamp without a reason, is a
    -- half-written invalidation. Both or neither.
    CONSTRAINT session_contexts_invalidation_pair_check
        CHECK ((invalidated_at IS NULL) = (invalidation_reason IS NULL)),

    CONSTRAINT session_contexts_score_range_check
        CHECK (device_trust_score BETWEEN 0 AND 100 AND adaptive_risk_score BETWEEN 0 AND 100)
);

-- "every session this principal held" — the revocation and incident query.
CREATE INDEX idx_session_contexts_principal
    ON session_contexts (principal_id, issued_at DESC);

-- "which session issued this JWT" — the incident-response entry point.
CREATE INDEX idx_session_contexts_jti
    ON session_contexts (envelope_jwt_jti);

-- "what is still live for this tenant" — partial, because invalidated and
-- expired rows accumulate indefinitely under the retention obligation and
-- would otherwise dominate the index.
CREATE INDEX idx_session_contexts_tenant_live
    ON session_contexts (tenant_id, expires_at)
    WHERE invalidated_at IS NULL;

-- Same tenant isolation as every other table here (000003). FORCE binds the
-- table owner too, so a superuser session in the SQL editor is subject to it as
-- well and must SET app.tenant_id before reading.
ALTER TABLE session_contexts ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_contexts FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON session_contexts
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
