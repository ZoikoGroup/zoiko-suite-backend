-- carta-svc: CARTA access assessments.
--
-- An assessment is the authoritative DECISION RECORD for one access-risk
-- evaluation: who asked for what, from which device/location, the trust score
-- the platform computed, the decision (ALLOW / STEP_UP_MFA / ISOLATE / DENY)
-- and the risk factors that moved the score. Doc 05 §5.4: "Session-trust
-- transitions must be evidentially recorded." Until this migration the records
-- lived in process memory and vanished on every restart (tracker rows 8g/93,
-- and the "no Postgres at all" note).
--
-- carta-svc keeps no other state: access policy is the scoring function in
-- internal/domain (code, not data) and trust signals arrive in the request
-- body. There is nothing cached here — every row is authoritative.
--
-- Records are append-only. The service exposes no update or delete, and the
-- trigger below makes that a property of the table rather than of the code, so
-- an evidence row cannot be silently rewritten by any role, including one that
-- holds UPDATE/DELETE grants.

CREATE TABLE IF NOT EXISTS carta_assessments (
    id                   UUID             PRIMARY KEY,
    tenant_id            VARCHAR(255)     NOT NULL CHECK (tenant_id <> ''),
    legal_entity_id      VARCHAR(255)     NOT NULL CHECK (legal_entity_id <> ''),
    subject_id           VARCHAR(255)     NOT NULL CHECK (subject_id <> ''),
    context              JSONB            NOT NULL,
    trust_score          DOUBLE PRECISION NOT NULL CHECK (trust_score >= 0 AND trust_score <= 100),
    risk_level           VARCHAR(16)      NOT NULL CHECK (risk_level IN ('LOW', 'MEDIUM', 'HIGH', 'CRITICAL')),
    decision             VARCHAR(16)      NOT NULL CHECK (decision IN ('ALLOW', 'STEP_UP_MFA', 'ISOLATE', 'DENY')),
    risk_factors         JSONB            NOT NULL DEFAULT '[]'::jsonb,
    assessment_timestamp TIMESTAMPTZ      NOT NULL,
    created_at           TIMESTAMPTZ      NOT NULL DEFAULT now()
);

-- ListAssessments: tenant + legal entity (mandatory scope), optional subject,
-- newest first.
CREATE INDEX IF NOT EXISTS idx_carta_assessments_scope
    ON carta_assessments (tenant_id, legal_entity_id, subject_id, assessment_timestamp DESC);

ALTER TABLE carta_assessments ENABLE ROW LEVEL SECURITY;
ALTER TABLE carta_assessments FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS carta_assessments_tenant_isolation ON carta_assessments;
CREATE POLICY carta_assessments_tenant_isolation ON carta_assessments FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION carta_assessments_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'carta_assessments is append-only: % is not permitted', TG_OP
        USING ERRCODE = 'insufficient_privilege';
END;
$$;

DROP TRIGGER IF EXISTS carta_assessments_no_update_delete ON carta_assessments;
CREATE TRIGGER carta_assessments_no_update_delete
    BEFORE UPDATE OR DELETE ON carta_assessments
    FOR EACH ROW EXECUTE FUNCTION carta_assessments_append_only();
