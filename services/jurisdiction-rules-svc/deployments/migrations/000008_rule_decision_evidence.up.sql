-- 000008_rule_decision_evidence.up.sql
-- ZS-JUR-001 Wave 2: RuleDecisionEvidence (s27, s30). Every runtime rule
-- resolution is recorded with the exact pack version, artifact digest, rule
-- version and sources it rested on, so a past outcome can be reconstructed
-- backwards: outcome -> rule version -> pack release -> interpretation -> source.
-- Append-only, like all evidence in this service.

CREATE TABLE rule_decision_evidence (
    decision_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    requested_by       TEXT         NOT NULL,
    correlation_id     TEXT,
    idempotency_key    TEXT,
    -- sha256 of the canonical request; ties a replay to the same question.
    request_digest     VARCHAR(71)  NOT NULL,
    request            JSONB        NOT NULL,
    effective_at       TIMESTAMPTZ  NOT NULL,
    outcome            VARCHAR(32)  NOT NULL,
    pack_ref           VARCHAR(128),
    pack_version       VARCHAR(32),
    pack_version_id    UUID         REFERENCES jurisdiction_pack_versions(pack_version_id),
    artifact_digest    VARCHAR(71),
    certification_id   UUID         REFERENCES pack_certifications(certification_id),
    rule_id            UUID,
    rule_content_digest VARCHAR(71),
    basis              VARCHAR(32),
    -- The full response returned to the caller, so a replay is byte-identical.
    response           JSONB        NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT ck_decision_request_digest CHECK (request_digest ~ '^sha256:[0-9a-f]{64}$'),
    CONSTRAINT ck_decision_resolved_has_basis CHECK (
        outcome <> 'RESOLVED' OR (pack_version_id IS NOT NULL AND artifact_digest IS NOT NULL AND rule_id IS NOT NULL AND rule_content_digest IS NOT NULL))
);

-- One decision per (caller, idempotency key).
CREATE UNIQUE INDEX uq_rule_decision_idempotency
    ON rule_decision_evidence (requested_by, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX idx_rule_decision_pack ON rule_decision_evidence (pack_version_id, created_at DESC);
CREATE INDEX idx_rule_decision_rule ON rule_decision_evidence (rule_id, created_at DESC);
CREATE INDEX idx_rule_decision_outcome ON rule_decision_evidence (outcome, created_at DESC);

CREATE TRIGGER trg_rule_decision_append_only BEFORE UPDATE ON rule_decision_evidence
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_update();
CREATE TRIGGER trg_rule_decision_no_delete BEFORE DELETE ON rule_decision_evidence
    FOR EACH ROW EXECUTE FUNCTION jur_forbid_delete();
