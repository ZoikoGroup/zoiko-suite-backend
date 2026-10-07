-- 000025_regulated_notices.up.sql
-- ZS-SVC-Y-001 NCD-05 (sections 8.1 to 8.5): the regulated notice package, its lifecycle,
-- acknowledgement and corrections.
--
-- What this proves, and what it never claims (8.5): NCD can prove what the platform
-- prepared, attempted, transmitted, observed and received as acknowledgement. Whether that
-- is legally effective service is decided by policy outside this service, so there is no
-- status here that means "legal service complete".
--
-- regulated_notices      one row per exact notice VERSION. Content is immutable once
--                        written; only the lifecycle status and the links move.
-- regulated_notice_events  append-only history of every transition, with its reason.
-- notice_acknowledgements  append-only recipient responses, one per notice version.

CREATE TABLE IF NOT EXISTS regulated_notices (
    notice_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              VARCHAR(255) NOT NULL,
    legal_entity_id        VARCHAR(255) NOT NULL,
    lineage_id             UUID NOT NULL,
    version_number         INTEGER NOT NULL,
    supersedes_notice_id   UUID REFERENCES regulated_notices (notice_id),
    correction_reason      TEXT,
    intent_version_id      UUID NOT NULL REFERENCES communication_intent_versions (version_id),
    recipient_principal_id VARCHAR(255) NOT NULL,
    recipient_capacity     VARCHAR(16) NOT NULL DEFAULT 'SELF',
    channel                VARCHAR(16) NOT NULL DEFAULT 'EMAIL',
    locale                 VARCHAR(16) NOT NULL DEFAULT 'en',
    subject                TEXT NOT NULL,
    body                   TEXT NOT NULL,
    content_hash           CHAR(64) NOT NULL,
    policy_ref             VARCHAR(255) NOT NULL,
    effective_date         DATE,
    ack_requirement        VARCHAR(24) NOT NULL,
    deadline_at            TIMESTAMPTZ,
    status                 VARCHAR(24) NOT NULL DEFAULT 'PREPARED',
    notification_id        UUID REFERENCES notifications (notification_id),
    superseded_by_notice_id UUID REFERENCES regulated_notices (notice_id),
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_notice_version UNIQUE (lineage_id, version_number),
    CONSTRAINT ck_notice_version_positive CHECK (version_number >= 1),
    CONSTRAINT ck_notice_first_has_no_prior CHECK ((version_number = 1) = (supersedes_notice_id IS NULL)),
    CONSTRAINT ck_notice_correction_has_reason CHECK (supersedes_notice_id IS NULL OR (correction_reason IS NOT NULL AND length(btrim(correction_reason)) > 0)),
    CONSTRAINT ck_notice_capacity CHECK (recipient_capacity IN ('SELF')),
    CONSTRAINT ck_notice_channel CHECK (channel IN ('EMAIL')),
    CONSTRAINT ck_notice_ack CHECK (ack_requirement IN ('NONE', 'RECEIPT', 'ACCEPTANCE_DECLINE')),
    CONSTRAINT ck_notice_deadline_when_ack CHECK (ack_requirement = 'NONE' OR deadline_at IS NOT NULL),
    CONSTRAINT ck_notice_hash CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT ck_notice_content_present CHECK (length(btrim(subject)) > 0 AND length(btrim(body)) > 0),
    CONSTRAINT ck_notice_policy_ref CHECK (length(btrim(policy_ref)) > 0),
    CONSTRAINT ck_notice_status CHECK (status IN ('PREPARED','READY','DELIVERY_IN_PROGRESS','DELIVERY_EVIDENCED','EXCEPTION',
        'SATISFIED_BY_POLICY','ACK_PENDING','ACKNOWLEDGED','DECLINED','EXPIRED','DISPUTED'))
);

CREATE INDEX IF NOT EXISTS idx_notices_lineage ON regulated_notices (tenant_id, lineage_id, version_number);
CREATE INDEX IF NOT EXISTS idx_notices_open ON regulated_notices (status) WHERE status IN ('DELIVERY_IN_PROGRESS', 'ACK_PENDING');

ALTER TABLE regulated_notices ENABLE ROW LEVEL SECURITY;
ALTER TABLE regulated_notices FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS notices_tenant_isolation ON regulated_notices;
CREATE POLICY notices_tenant_isolation ON regulated_notices FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
-- The sweeper discovers open notices across tenants, read-only, then acts under each tenant.
DROP POLICY IF EXISTS notices_platform_scope_read ON regulated_notices;
CREATE POLICY notices_platform_scope_read ON regulated_notices FOR SELECT
    USING (current_setting('app.platform_scope', true) = 'true');

-- Content is the evidence of what was served: it never changes. Only the lifecycle status
-- and the two links move, and the status moves along the allowed edges only.
CREATE OR REPLACE FUNCTION guard_regulated_notice() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'regulated notices are never deleted' USING ERRCODE = '23514';
    END IF;
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'PREPARED' THEN
            RAISE EXCEPTION 'a notice is created PREPARED, not %', NEW.status USING ERRCODE = '23514';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.notice_id IS DISTINCT FROM OLD.notice_id OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
       OR NEW.legal_entity_id IS DISTINCT FROM OLD.legal_entity_id OR NEW.lineage_id IS DISTINCT FROM OLD.lineage_id
       OR NEW.version_number IS DISTINCT FROM OLD.version_number OR NEW.supersedes_notice_id IS DISTINCT FROM OLD.supersedes_notice_id
       OR NEW.correction_reason IS DISTINCT FROM OLD.correction_reason OR NEW.intent_version_id IS DISTINCT FROM OLD.intent_version_id
       OR NEW.recipient_principal_id IS DISTINCT FROM OLD.recipient_principal_id OR NEW.recipient_capacity IS DISTINCT FROM OLD.recipient_capacity
       OR NEW.channel IS DISTINCT FROM OLD.channel OR NEW.locale IS DISTINCT FROM OLD.locale
       OR NEW.subject IS DISTINCT FROM OLD.subject OR NEW.body IS DISTINCT FROM OLD.body OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
       OR NEW.policy_ref IS DISTINCT FROM OLD.policy_ref OR NEW.effective_date IS DISTINCT FROM OLD.effective_date
       OR NEW.ack_requirement IS DISTINCT FROM OLD.ack_requirement OR NEW.deadline_at IS DISTINCT FROM OLD.deadline_at
       OR NEW.created_by_principal_id IS DISTINCT FROM OLD.created_by_principal_id OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'notice % is immutable; correct it with a new version', OLD.notice_id USING ERRCODE = '23514';
    END IF;
    IF OLD.notification_id IS NOT NULL AND NEW.notification_id IS DISTINCT FROM OLD.notification_id THEN
        RAISE EXCEPTION 'notice % is already linked to its delivery', OLD.notice_id USING ERRCODE = '23514';
    END IF;
    IF OLD.superseded_by_notice_id IS NOT NULL AND NEW.superseded_by_notice_id IS DISTINCT FROM OLD.superseded_by_notice_id THEN
        RAISE EXCEPTION 'notice % is already superseded', OLD.notice_id USING ERRCODE = '23514';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status AND NOT (
        (OLD.status = 'PREPARED'             AND NEW.status = 'READY') OR
        (OLD.status = 'READY'                AND NEW.status = 'DELIVERY_IN_PROGRESS' AND NEW.notification_id IS NOT NULL) OR
        (OLD.status = 'DELIVERY_IN_PROGRESS' AND NEW.status IN ('DELIVERY_EVIDENCED', 'EXCEPTION')) OR
        (OLD.status = 'DELIVERY_EVIDENCED'   AND NEW.status IN ('SATISFIED_BY_POLICY', 'ACK_PENDING')) OR
        (OLD.status = 'ACK_PENDING'          AND NEW.status IN ('ACKNOWLEDGED', 'DECLINED', 'DISPUTED', 'EXPIRED'))) THEN
        RAISE EXCEPTION 'notice % cannot move from % to %', OLD.notice_id, OLD.status, NEW.status USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_guard_regulated_notice ON regulated_notices;
CREATE TRIGGER trg_guard_regulated_notice BEFORE INSERT OR UPDATE OR DELETE ON regulated_notices
    FOR EACH ROW EXECUTE FUNCTION guard_regulated_notice();

-- Append-only history of every transition.
CREATE TABLE IF NOT EXISTS regulated_notice_events (
    event_id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           VARCHAR(255) NOT NULL,
    notice_id           UUID NOT NULL REFERENCES regulated_notices (notice_id),
    from_status         VARCHAR(24),
    to_status           VARCHAR(24) NOT NULL,
    actor_principal_id  VARCHAR(255) NOT NULL,
    reason              TEXT NOT NULL,
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notice_events ON regulated_notice_events (tenant_id, notice_id, occurred_at);

-- A recipient's response to one notice version. Only an authenticated action by the
-- recipient is a method here: an email open or a link click is never an acknowledgement.
CREATE TABLE IF NOT EXISTS notice_acknowledgements (
    ack_id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           VARCHAR(255) NOT NULL,
    notice_id           UUID NOT NULL REFERENCES regulated_notices (notice_id),
    action              VARCHAR(16) NOT NULL,
    actor_principal_id  VARCHAR(255) NOT NULL,
    method              VARCHAR(32) NOT NULL DEFAULT 'AUTHENTICATED_ACTION',
    comment             VARCHAR(500),
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ck_ack_action CHECK (action IN ('ACKNOWLEDGE', 'ACCEPT', 'DECLINE', 'DISPUTE')),
    CONSTRAINT ck_ack_method CHECK (method IN ('AUTHENTICATED_ACTION')),
    CONSTRAINT uq_ack_one_response UNIQUE (notice_id)
);

ALTER TABLE regulated_notice_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE regulated_notice_events FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS notice_events_tenant_isolation ON regulated_notice_events;
CREATE POLICY notice_events_tenant_isolation ON regulated_notice_events FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE notice_acknowledgements ENABLE ROW LEVEL SECURITY;
ALTER TABLE notice_acknowledgements FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS notice_acks_tenant_isolation ON notice_acknowledgements;
CREATE POLICY notice_acks_tenant_isolation ON notice_acknowledgements FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

CREATE OR REPLACE FUNCTION guard_notice_append_only() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_notice_events_append_only ON regulated_notice_events;
CREATE TRIGGER trg_notice_events_append_only BEFORE UPDATE OR DELETE ON regulated_notice_events
    FOR EACH ROW EXECUTE FUNCTION guard_notice_append_only();
DROP TRIGGER IF EXISTS trg_notice_acks_append_only ON notice_acknowledgements;
CREATE TRIGGER trg_notice_acks_append_only BEFORE UPDATE OR DELETE ON notice_acknowledgements
    FOR EACH ROW EXECUTE FUNCTION guard_notice_append_only();

-- Notice events are enqueued with the transition that causes them.
ALTER TABLE event_outbox DROP CONSTRAINT IF EXISTS event_outbox_event_known;
ALTER TABLE event_outbox ADD CONSTRAINT event_outbox_event_known
    CHECK (event_type IN (
        'notification.sent',
        'notification.failed',
        'notification.outcome_unknown',
        'template.created',
        'template.version_approved',
        'template.published',
        'template.retired',
        'delivery.attempt.created',
        'delivery.attempt.unknown',
        'notice.dispatched',
        'notice.delivery_evidenced',
        'notice.exception',
        'notice.acknowledged',
        'notice.declined',
        'notice.disputed',
        'notice.expired',
        'notice.corrected'
    ));
