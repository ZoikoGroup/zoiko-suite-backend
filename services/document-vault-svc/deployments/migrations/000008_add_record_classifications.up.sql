-- BIZ-02 Record Classification, Wave 1: schema + domain model.
--
-- A separate governed aggregate, not a mutable column on documents (per
-- the doc's own "record classification... does not grant access itself"
-- framing and its distinct command/query/event catalogue). documents.
-- classification (migration 000001) remains a cached snapshot of the
-- current CONFIRMED value for backward-compatible reads — this table is
-- the actual source of truth and full history.
--
-- Lifecycle: Unclassified (no row yet) -> Candidate -> Confirmed ->
-- Superseded/Restricted. ClassifyRecord/Reclassify insert a CANDIDATE
-- row; ConfirmClassification/SupersedeClassification move one to
-- CONFIRMED (and, for a reclassification, forward-link the prior
-- CONFIRMED row to SUPERSEDED in the same step). RESTRICTED is reached
-- from CONFIRMED only when classification_value=RESTRICTED, gated by an
-- additional security/privacy-owner authorization per the doc's SoD
-- line -- wired in a later wave, not this one.
--
-- source/confidence: AI-sourced proposals carry a numeric confidence;
-- human proposals do not (a human's judgment isn't expressed as a
-- probability the way a classifier's is) -- enforced below, not left
-- to convention.
CREATE TABLE record_classifications (
    classification_id      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id             UUID NOT NULL REFERENCES documents(document_id),
    tenant_id                UUID NOT NULL,
    legal_entity_id          UUID NOT NULL,
    classification_value    VARCHAR(20) NOT NULL
        CHECK (classification_value IN ('PUBLIC', 'INTERNAL', 'CONFIDENTIAL', 'RESTRICTED')),
    status                   VARCHAR(20) NOT NULL DEFAULT 'CANDIDATE'
        CHECK (status IN ('CANDIDATE', 'CONFIRMED', 'SUPERSEDED', 'RESTRICTED')),
    source                   VARCHAR(10) NOT NULL CHECK (source IN ('HUMAN', 'AI')),
    -- Confidence policy (threshold below which an AI proposal cannot
    -- auto-confirm) is a real, wired mechanism -- see
    -- internal/domain/classification.go's own doc comment for why the
    -- actual threshold value is an explicit placeholder pending AI-02
    -- integration, not invented here.
    confidence               NUMERIC(5,4) CHECK (confidence IS NULL OR (confidence >= 0 AND confidence <= 1)),
    rule_model_version       TEXT,
    source_evidence          TEXT,
    proposed_by_principal_id VARCHAR(255) NOT NULL CHECK (proposed_by_principal_id <> ''),
    proposed_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_by_principal_id VARCHAR(255),
    confirmed_at              TIMESTAMPTZ,
    superseded_by_classification_id UUID REFERENCES record_classifications(classification_id),
    effective_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    correlation_id            VARCHAR(255),
    CHECK (source = 'AI' AND confidence IS NOT NULL OR source = 'HUMAN' AND confidence IS NULL),
    CHECK (
        (confirmed_at IS NULL AND confirmed_by_principal_id IS NULL)
        OR (confirmed_at IS NOT NULL AND confirmed_by_principal_id IS NOT NULL)
    ),
    -- Maker-checker: a human's own proposal cannot be confirmed by that
    -- same human. Does not apply to AI-sourced proposals, which have no
    -- personal "maker" to self-approve.
    CHECK (
        source = 'AI'
        OR confirmed_by_principal_id IS NULL
        OR confirmed_by_principal_id <> proposed_by_principal_id
    )
);

CREATE INDEX idx_record_classifications_document ON record_classifications (document_id, proposed_at DESC);
CREATE INDEX idx_record_classifications_tenant_status ON record_classifications (tenant_id, status);

ALTER TABLE record_classifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE record_classifications FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation_policy ON record_classifications
    FOR ALL USING (tenant_id::text = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id::text = current_setting('app.tenant_id', true));

-- Same immutability posture as every other governed table in this
-- service: the FACTS of a proposal (value/source/confidence/evidence)
-- are permanent once written. status/confirmed_*/superseded_by may
-- change, but only forward, and only once each.
CREATE OR REPLACE FUNCTION reject_classification_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'record_classifications rows are never deleted';
    END IF;
    IF NEW.document_id IS DISTINCT FROM OLD.document_id
        OR NEW.classification_value IS DISTINCT FROM OLD.classification_value
        OR NEW.source IS DISTINCT FROM OLD.source
        OR NEW.confidence IS DISTINCT FROM OLD.confidence
        OR NEW.proposed_by_principal_id IS DISTINCT FROM OLD.proposed_by_principal_id
        OR NEW.proposed_at IS DISTINCT FROM OLD.proposed_at
    THEN
        RAISE EXCEPTION 'record_classifications proposal facts are immutable once written (classification %)', OLD.classification_id;
    END IF;
    IF OLD.confirmed_at IS NOT NULL AND (
        NEW.confirmed_at IS DISTINCT FROM OLD.confirmed_at
        OR NEW.confirmed_by_principal_id IS DISTINCT FROM OLD.confirmed_by_principal_id
    ) THEN
        RAISE EXCEPTION 'classification % is already confirmed; confirmation cannot be changed or reversed', OLD.classification_id;
    END IF;
    IF OLD.superseded_by_classification_id IS NOT NULL
        AND NEW.superseded_by_classification_id IS DISTINCT FROM OLD.superseded_by_classification_id THEN
        RAISE EXCEPTION 'classification % has already been superseded by %; this cannot be changed', OLD.classification_id, OLD.superseded_by_classification_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_reject_classification_mutation
    BEFORE UPDATE OR DELETE ON record_classifications
    FOR EACH ROW EXECUTE FUNCTION reject_classification_mutation();
