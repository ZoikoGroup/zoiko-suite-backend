-- Migration: 003_add_drc05_signature_orchestration.sql
--
-- DRC-05 Signature, Seal & Attestation Orchestrator (ZS-SVC-S-001 §7).
-- Retrofits this service's existing signature_envelopes table (001_init.sql)
-- rather than building a new service — this IS the provider-neutral
-- e-signature orchestrator the spec describes, just not yet built out past
-- a bare envelope-status tracker.
--
-- Three things happen here:
--   1. signature_profiles: the governed contract (assurance level,
--      jurisdiction, identity/witness requirements) an envelope is signed
--      under — did not exist at all before this migration.
--   2. provider_attempts, participants, completion_evidence: the
--      orthogonal state dimensions DRC-05 requires, kept as their OWN
--      tables rather than more columns crammed onto signature_envelopes —
--      an envelope's lifecycle, a provider call's outcome, a signer's
--      progress and the sealed completion record are genuinely
--      independent facts that each need their own forward-only history.
--   3. A real state-machine trigger on signature_envelopes.status,
--      replacing the free-form write UpdateEnvelopeStatus has always had.
--      Before this, any status could follow any other status — "VOIDED"
--      could even be set AFTER "SIGNED", which is exactly the amendment/
--      voiding violation DRC-05 exists to prevent. The trigger makes
--      voiding only reachable from a non-terminal state, enforced at the
--      database, not merely by handler-side convention.

-- ── Signature Profiles ───────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS signature_profiles (
    profile_id              VARCHAR(64) PRIMARY KEY,
    tenant_id                VARCHAR(64) NOT NULL,
    legal_entity_id          VARCHAR(64) NOT NULL,
    assurance_level          VARCHAR(16) NOT NULL
        CHECK (assurance_level IN ('SES', 'AES', 'QES')),
    jurisdiction              VARCHAR(64) NOT NULL,
    identity_requirement      VARCHAR(32) NOT NULL
        CHECK (identity_requirement IN ('EMAIL_ONLY', 'KBA', 'GOV_ID', 'VIDEO_ID')),
    witness_required          BOOLEAN NOT NULL DEFAULT false,
    status                    VARCHAR(16) NOT NULL DEFAULT 'DRAFT'
        CHECK (status IN ('DRAFT', 'ACTIVE', 'RETIRED')),
    created_by_principal_id   VARCHAR(255) NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    activated_at              TIMESTAMPTZ,
    retired_at                TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_signature_profiles_tenant ON signature_profiles(tenant_id, status);

ALTER TABLE signature_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE signature_profiles FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON signature_profiles
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

-- DRAFT may be edited freely (nothing relies on it yet); once ACTIVE its
-- terms are permanent, same maker-checker-adjacent doctrine as every other
-- governed-rule table this estate has added (retention_rule_versions,
-- record_classifications): a contract's terms must be fixed before
-- anything is signed under it.
CREATE OR REPLACE FUNCTION reject_signature_profile_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'signature_profiles rows are never deleted';
    END IF;
    IF OLD.status <> 'DRAFT' THEN
        IF NEW.assurance_level IS DISTINCT FROM OLD.assurance_level
            OR NEW.jurisdiction IS DISTINCT FROM OLD.jurisdiction
            OR NEW.identity_requirement IS DISTINCT FROM OLD.identity_requirement
            OR NEW.witness_required IS DISTINCT FROM OLD.witness_required
        THEN
            RAISE EXCEPTION 'signature_profile % is no longer DRAFT; its terms are immutable', OLD.profile_id;
        END IF;
    END IF;
    IF OLD.status = 'RETIRED' AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'signature_profile % is RETIRED and immutable', OLD.profile_id;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'DRAFT' THEN
                IF NEW.status <> 'ACTIVE' THEN
                    RAISE EXCEPTION 'invalid signature_profile transition from DRAFT to %', NEW.status;
                END IF;
            WHEN 'ACTIVE' THEN
                IF NEW.status <> 'RETIRED' THEN
                    RAISE EXCEPTION 'invalid signature_profile transition from ACTIVE to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown signature_profile status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_signature_profile_mutation
    BEFORE UPDATE OR DELETE ON signature_profiles
    FOR EACH ROW EXECUTE FUNCTION reject_signature_profile_mutation();

-- ── Envelope additions ───────────────────────────────────────────────────────

ALTER TABLE signature_envelopes ADD COLUMN IF NOT EXISTS signature_profile_id VARCHAR(64) REFERENCES signature_profiles(profile_id);
ALTER TABLE signature_envelopes ADD COLUMN IF NOT EXISTS supersedes_envelope_id VARCHAR(64) REFERENCES signature_envelopes(envelope_id);

-- Forward-only envelope lifecycle. Before this trigger, UpdateEnvelopeStatus
-- was a free write — any status could follow any other, so a "SIGNED"
-- envelope could be silently reset or a "VOIDED" one revived. Voiding is
-- now only reachable from a non-terminal state (DRC-05's own amendment/
-- voiding rule), and SIGNED is genuinely terminal at the envelope level —
-- completion_evidence below is what then seals the actual outcome.
CREATE OR REPLACE FUNCTION reject_signature_envelope_invalid_transition() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('SIGNED', 'VOIDED') AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'signature_envelope % is % and its status is terminal', OLD.envelope_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'PENDING' THEN
                IF NEW.status NOT IN ('SENT', 'VOIDED') THEN
                    RAISE EXCEPTION 'invalid signature_envelope transition from PENDING to %', NEW.status;
                END IF;
            WHEN 'SENT' THEN
                IF NEW.status NOT IN ('DELIVERED', 'VOIDED') THEN
                    RAISE EXCEPTION 'invalid signature_envelope transition from SENT to %', NEW.status;
                END IF;
            WHEN 'DELIVERED' THEN
                IF NEW.status NOT IN ('SIGNED', 'VOIDED') THEN
                    RAISE EXCEPTION 'invalid signature_envelope transition from DELIVERED to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown signature_envelope status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_signature_envelope_invalid_transition
    BEFORE UPDATE ON signature_envelopes
    FOR EACH ROW EXECUTE FUNCTION reject_signature_envelope_invalid_transition();

-- ── Provider Attempts ────────────────────────────────────────────────────────

-- One row per outbound call to the signing provider. idempotency_key lets a
-- retried call return the SAME attempt rather than creating a duplicate
-- side effect; UNKNOWN can only be exited via an explicit reconciliation
-- (checking authoritative provider status), never by blindly retrying the
-- original call — the whole point of tracking attempts as their own
-- durable record instead of a bare envelope.status write.
CREATE TABLE IF NOT EXISTS provider_attempts (
    attempt_id                VARCHAR(64) PRIMARY KEY,
    tenant_id                  VARCHAR(64) NOT NULL,
    envelope_id                VARCHAR(64) NOT NULL REFERENCES signature_envelopes(envelope_id),
    idempotency_key             VARCHAR(255) NOT NULL,
    provider                    VARCHAR(64) NOT NULL,
    attempted_action            VARCHAR(16) NOT NULL
        CHECK (attempted_action IN ('SEND', 'VOID', 'CHECK_STATUS')),
    outcome                     VARCHAR(16) NOT NULL DEFAULT 'PENDING'
        CHECK (outcome IN ('PENDING', 'SUCCEEDED', 'FAILED', 'UNKNOWN')),
    provider_response_ref       VARCHAR(256),
    error_detail                TEXT,
    attempted_at                TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    resolved_at                 TIMESTAMPTZ,
    resolved_by_principal_id    VARCHAR(255),
    CONSTRAINT provider_attempts_idempotent UNIQUE (envelope_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_provider_attempts_envelope ON provider_attempts(envelope_id);
CREATE INDEX IF NOT EXISTS idx_provider_attempts_tenant_outcome ON provider_attempts(tenant_id, outcome);

ALTER TABLE provider_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE provider_attempts FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON provider_attempts
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE OR REPLACE FUNCTION reject_provider_attempt_invalid_transition() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'provider_attempts rows are never deleted';
    END IF;
    IF NEW.envelope_id IS DISTINCT FROM OLD.envelope_id
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.attempted_action IS DISTINCT FROM OLD.attempted_action
        OR NEW.attempted_at IS DISTINCT FROM OLD.attempted_at
    THEN
        RAISE EXCEPTION 'provider_attempt % facts are immutable', OLD.attempt_id;
    END IF;
    IF OLD.outcome IN ('SUCCEEDED', 'FAILED') AND NEW.outcome IS DISTINCT FROM OLD.outcome THEN
        RAISE EXCEPTION 'provider_attempt % is % and immutable', OLD.attempt_id, OLD.outcome;
    END IF;
    IF OLD.outcome <> NEW.outcome THEN
        CASE OLD.outcome
            WHEN 'PENDING' THEN
                IF NEW.outcome NOT IN ('SUCCEEDED', 'FAILED', 'UNKNOWN') THEN
                    RAISE EXCEPTION 'invalid provider_attempt transition from PENDING to %', NEW.outcome;
                END IF;
            WHEN 'UNKNOWN' THEN
                IF NEW.outcome NOT IN ('SUCCEEDED', 'FAILED') THEN
                    RAISE EXCEPTION 'invalid provider_attempt transition from UNKNOWN to % (must be reconciled)', NEW.outcome;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown provider_attempt outcome %', OLD.outcome;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_provider_attempt_invalid_transition
    BEFORE UPDATE OR DELETE ON provider_attempts
    FOR EACH ROW EXECUTE FUNCTION reject_provider_attempt_invalid_transition();

-- ── Participants ─────────────────────────────────────────────────────────────

-- Per-signer progress, independent of the envelope's own status — "Alice
-- signed, Bob hasn't" is not representable with a single envelope-level
-- status field.
CREATE TABLE IF NOT EXISTS participants (
    participant_id           VARCHAR(64) PRIMARY KEY,
    tenant_id                 VARCHAR(64) NOT NULL,
    envelope_id                VARCHAR(64) NOT NULL REFERENCES signature_envelopes(envelope_id),
    email                      VARCHAR(256) NOT NULL,
    name                       VARCHAR(256) NOT NULL,
    role                       VARCHAR(16) NOT NULL DEFAULT 'SIGNER'
        CHECK (role IN ('SIGNER', 'WITNESS', 'APPROVER', 'CC')),
    participant_state          VARCHAR(16) NOT NULL DEFAULT 'INVITED'
        CHECK (participant_state IN ('INVITED', 'VIEWED', 'SIGNED', 'DECLINED')),
    invited_at                 TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    viewed_at                  TIMESTAMPTZ,
    signed_at                  TIMESTAMPTZ,
    declined_at                TIMESTAMPTZ,
    decline_reason             TEXT
);

CREATE INDEX IF NOT EXISTS idx_participants_envelope ON participants(envelope_id);

ALTER TABLE participants ENABLE ROW LEVEL SECURITY;
ALTER TABLE participants FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON participants
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE OR REPLACE FUNCTION reject_participant_invalid_transition() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'participants rows are never deleted';
    END IF;
    IF NEW.envelope_id IS DISTINCT FROM OLD.envelope_id OR NEW.email IS DISTINCT FROM OLD.email THEN
        RAISE EXCEPTION 'participant % envelope/email binding is immutable', OLD.participant_id;
    END IF;
    IF OLD.participant_state IN ('SIGNED', 'DECLINED') AND NEW.participant_state IS DISTINCT FROM OLD.participant_state THEN
        RAISE EXCEPTION 'participant % is % and immutable', OLD.participant_id, OLD.participant_state;
    END IF;
    IF OLD.participant_state <> NEW.participant_state THEN
        CASE OLD.participant_state
            WHEN 'INVITED' THEN
                IF NEW.participant_state NOT IN ('VIEWED', 'DECLINED') THEN
                    RAISE EXCEPTION 'invalid participant transition from INVITED to %', NEW.participant_state;
                END IF;
            WHEN 'VIEWED' THEN
                IF NEW.participant_state NOT IN ('SIGNED', 'DECLINED') THEN
                    RAISE EXCEPTION 'invalid participant transition from VIEWED to %', NEW.participant_state;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown participant_state %', OLD.participant_state;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_participant_invalid_transition
    BEFORE UPDATE OR DELETE ON participants
    FOR EACH ROW EXECUTE FUNCTION reject_participant_invalid_transition();

-- ── Completion Evidence ──────────────────────────────────────────────────────

-- The sealed, durable record of what the provider actually returned once
-- an envelope is SIGNED — one per envelope, never mutated or deleted once
-- written, same doctrine as every other evidence/manifest table this
-- estate has added.
CREATE TABLE IF NOT EXISTS completion_evidence (
    evidence_id                  VARCHAR(64) PRIMARY KEY,
    tenant_id                     VARCHAR(64) NOT NULL,
    envelope_id                   VARCHAR(64) NOT NULL UNIQUE REFERENCES signature_envelopes(envelope_id),
    completion_certificate_ref    VARCHAR(256) NOT NULL,
    completed_artifact_hash       VARCHAR(64) NOT NULL CHECK (length(completed_artifact_hash) = 64),
    sealed_at                     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sealed_by_principal_id        VARCHAR(255) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_completion_evidence_tenant ON completion_evidence(tenant_id);

ALTER TABLE completion_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE completion_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON completion_evidence
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));

CREATE OR REPLACE FUNCTION reject_completion_evidence_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'completion_evidence rows are immutable and never deleted';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_completion_evidence_mutation
    BEFORE UPDATE OR DELETE ON completion_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_completion_evidence_mutation();
