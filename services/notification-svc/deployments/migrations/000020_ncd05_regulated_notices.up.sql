-- ZS-SVC-Y-001 NCD-05: Regulated Notice, Acknowledgment & Communication Record (§8).
--
-- §8.5 is the boundary this schema keeps: NCD proves what was prepared,
-- attempted, transmitted, observed and acknowledged. It never records "legal
-- service complete" — there is no such state below. The legal or business
-- disposition is a REFERENCE to the authority that decided it (PDC/WFC/domain).
CREATE TABLE IF NOT EXISTS ncd_regulated_notices (
    notice_id               UUID         NOT NULL,
    notice_version          INTEGER      NOT NULL CHECK (notice_version >= 1),
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    communication_id        UUID         NOT NULL UNIQUE REFERENCES ncd_communications (communication_id),
    -- PDC decision/rule reference: NCD does not invent service rules (§8.1).
    legal_basis_ref         VARCHAR(255) NOT NULL CHECK (legal_basis_ref <> ''),
    recipient_capacity      VARCHAR(120) NOT NULL,
    delivery_methods        JSONB        NOT NULL DEFAULT '[]'::jsonb,
    content_hash            VARCHAR(64)  NOT NULL,
    attachment_manifest     JSONB        NOT NULL DEFAULT '[]'::jsonb,
    locale                  VARCHAR(20)  NOT NULL,
    effective_date          DATE,
    -- WFC owns the clock; NCD holds the reference and the due time.
    wfc_obligation_ref      VARCHAR(255),
    deadline_at             TIMESTAMPTZ,
    ack_requirement         VARCHAR(30)  NOT NULL CHECK (ack_requirement IN ('NONE','RECEIPT','AUTHENTICATED_ACK','ACCEPTANCE')),
    evidence_requirement    VARCHAR(2)   NOT NULL,
    record_requirement      BOOLEAN      NOT NULL DEFAULT false,
    record_status           VARCHAR(20)  NOT NULL DEFAULT 'NOT_REQUIRED'
                                CHECK (record_status IN ('NOT_REQUIRED','PENDING','DECLARED')),
    drc_record_ref          VARCHAR(255),
    state                   VARCHAR(30)  NOT NULL DEFAULT 'PREPARED' CHECK (state IN (
                                'PREPARED','READY','DELIVERY_IN_PROGRESS','DELIVERY_EVIDENCED',
                                'SATISFIED_BY_POLICY','ACK_PENDING','ACKNOWLEDGED','DECLINED','EXPIRED','DISPUTED',
                                'EXCEPTION','SUPERSEDED')),
    at_risk_notified_at     TIMESTAMPTZ,
    supersedes_notice_id    UUID,
    supersession_reason     TEXT,
    disposition_ref         VARCHAR(255),
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    state_changed_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (notice_id, notice_version),
    CONSTRAINT ncd_notice_supersession_reason CHECK (supersedes_notice_id IS NULL OR (supersession_reason IS NOT NULL AND supersession_reason <> '')),
    CONSTRAINT ncd_notice_record_ref CHECK (record_status <> 'DECLARED' OR drc_record_ref IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_ncd_notices_deadline ON ncd_regulated_notices (deadline_at)
    WHERE state IN ('READY','DELIVERY_IN_PROGRESS','DELIVERY_EVIDENCED','ACK_PENDING');

-- The notice package is immutable: content hash, basis, requirement and
-- recipient capacity never change. A correction is a new version (§8.4).
CREATE OR REPLACE FUNCTION ncd_reject_notice_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'regulated notices are never deleted (§8.4: a mistaken recipient does not permit deleting evidence)';
    END IF;
    IF NEW.notice_id IS DISTINCT FROM OLD.notice_id
        OR NEW.notice_version IS DISTINCT FROM OLD.notice_version
        OR NEW.communication_id IS DISTINCT FROM OLD.communication_id
        OR NEW.legal_basis_ref IS DISTINCT FROM OLD.legal_basis_ref
        OR NEW.recipient_capacity IS DISTINCT FROM OLD.recipient_capacity
        OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
        OR NEW.attachment_manifest IS DISTINCT FROM OLD.attachment_manifest
        OR NEW.ack_requirement IS DISTINCT FROM OLD.ack_requirement
        OR NEW.evidence_requirement IS DISTINCT FROM OLD.evidence_requirement
        OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at
        OR NEW.supersedes_notice_id IS DISTINCT FROM OLD.supersedes_notice_id
    THEN
        RAISE EXCEPTION 'notice %/% package is immutable; issue a correction', OLD.notice_id, OLD.notice_version;
    END IF;
    IF OLD.drc_record_ref IS NOT NULL AND NEW.drc_record_ref IS DISTINCT FROM OLD.drc_record_ref THEN
        RAISE EXCEPTION 'notice %/% record declaration is immutable', OLD.notice_id, OLD.notice_version;
    END IF;
    IF NEW.state <> OLD.state THEN
        NEW.state_changed_at := now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ncd_reject_notice_mutation ON ncd_regulated_notices;
CREATE TRIGGER trg_ncd_reject_notice_mutation BEFORE UPDATE OR DELETE ON ncd_regulated_notices
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_notice_mutation();

-- §9.1 Acknowledgment: actor, method, exact content version, disposition,
-- timestamp, evidence reference (INV-21). Append-only.
CREATE TABLE IF NOT EXISTS ncd_acknowledgments (
    ack_id                  UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    notice_id               UUID         NOT NULL,
    notice_version          INTEGER      NOT NULL,
    communication_id        UUID         NOT NULL REFERENCES ncd_communications (communication_id),
    actor_principal_id      VARCHAR(255) NOT NULL,
    method                  VARCHAR(40)  NOT NULL CHECK (method IN ('AUTHENTICATED_IN_APP','AUTHENTICATED_PORTAL','OPERATOR_RECORDED')),
    disposition             VARCHAR(20)  NOT NULL CHECK (disposition IN ('ACKNOWLEDGED','DECLINED','DISPUTED')),
    -- The hash of the content the actor was shown. It must equal the notice's
    -- own content hash (NP-40): nobody acknowledges a version they did not see.
    content_hash            VARCHAR(64)  NOT NULL,
    evidence_ref            VARCHAR(500),
    comment                 TEXT,
    acknowledged_at         TIMESTAMPTZ  NOT NULL DEFAULT now(),
    FOREIGN KEY (notice_id, notice_version) REFERENCES ncd_regulated_notices (notice_id, notice_version)
);
CREATE INDEX IF NOT EXISTS idx_ncd_acks_notice ON ncd_acknowledgments (tenant_id, notice_id, notice_version);

DROP TRIGGER IF EXISTS trg_ncd_acks_append_only ON ncd_acknowledgments;
CREATE TRIGGER trg_ncd_acks_append_only BEFORE UPDATE OR DELETE ON ncd_acknowledgments
    FOR EACH ROW EXECUTE FUNCTION ncd_reject_evidence_mutation();

DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ncd_regulated_notices','ncd_acknowledgments']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_tenant', t);
        EXECUTE format('CREATE POLICY %I ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), ''''))
            WITH CHECK (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), ''''))', t || '_tenant', t);
    END LOOP;
END $$;

DROP POLICY IF EXISTS ncd_notices_platform_read ON ncd_regulated_notices;
CREATE POLICY ncd_notices_platform_read ON ncd_regulated_notices FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.platform_scope', true), ''), 'false') = 'true');
