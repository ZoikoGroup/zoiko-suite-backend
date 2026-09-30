-- ZS-SVC-Y-001 NCD-03: Delivery Orchestration, Provider Routing & Attempt (§6),
-- plus the §10.1 Communication aggregate the whole plane hangs off.

-- ── Communication (§9.1) ────────────────────────────────────────────────────
-- One logical business communication (INV-02). Retries, fallbacks and resends
-- are attempts and jobs UNDER it, never new communications.
CREATE TABLE IF NOT EXISTS ncd_communications (
    communication_id          UUID         PRIMARY KEY,
    tenant_id                 VARCHAR(255) NOT NULL,
    legal_entity_id           VARCHAR(255) NOT NULL,
    intent_id                 UUID         NOT NULL,
    -- Pinned at prepare (INV-04); NULL until then.
    intent_version            INTEGER,
    purpose_class             VARCHAR(40),
    -- §3.4 purpose-scoped idempotency key, derived from the originating
    -- business event, the intent and the recipient.
    idempotency_key           VARCHAR(128) NOT NULL,
    source_event_id           VARCHAR(255) NOT NULL,
    source_event_type         VARCHAR(120),
    workflow_id               VARCHAR(255),
    recipient_principal_id    VARCHAR(255) NOT NULL,
    recipient_tenant_id       VARCHAR(255),
    -- A caller-supplied endpoint, only under a controlled exception (§5.2).
    free_text_endpoint        JSONB,
    locale                    VARCHAR(20)  NOT NULL,
    variables                 JSONB        NOT NULL DEFAULT '{}'::jsonb,
    -- [{slot, drc_record_id, drc_version, sha256}] pinned at creation (INV-18).
    attachments               JSONB        NOT NULL DEFAULT '[]'::jsonb,
    -- Policy snapshot (§3.2): PRV/PDC decisions as referenced, not re-derived.
    privacy_permission        JSONB        NOT NULL DEFAULT '{}'::jsonb,
    marketing_permission      JSONB        NOT NULL DEFAULT '{}'::jsonb,
    pdc_decision_ref          VARCHAR(255),
    residency_regions         TEXT[]       NOT NULL DEFAULT '{}',
    lifecycle_state           VARCHAR(30)  NOT NULL DEFAULT 'CREATED' CHECK (lifecycle_state IN (
                                  'CREATED','PREPARED','BLOCKED','REVIEW_REQUIRED','DISPATCHED',
                                  'COMPLETED','EXCEPTION','EXPIRED','CANCELLED')),
    blocked_reason_code       VARCHAR(10),
    blocked_detail            TEXT,
    recipient_plan_id         UUID,
    channel_decision_id       UUID,
    not_before                TIMESTAMPTZ,
    expires_at                TIMESTAMPTZ  NOT NULL,
    priority                  INTEGER      NOT NULL DEFAULT 0,
    -- §6.6 / INV-19: a correction is a NEW communication linked to the old.
    supersedes_communication_id UUID REFERENCES ncd_communications (communication_id),
    correction_reason         TEXT,
    bulk_id                   UUID,
    created_by_principal_id   VARCHAR(255) NOT NULL,
    created_at                TIMESTAMPTZ  NOT NULL DEFAULT now(),
    prepared_at               TIMESTAMPTZ,
    dispatched_at             TIMESTAMPTZ,
    concluded_at              TIMESTAMPTZ,
    CONSTRAINT ncd_comm_correction_has_reason CHECK (supersedes_communication_id IS NULL OR (correction_reason IS NOT NULL AND correction_reason <> '')),
    CONSTRAINT ncd_comm_blocked_has_code CHECK (lifecycle_state NOT IN ('BLOCKED','REVIEW_REQUIRED') OR blocked_reason_code IS NOT NULL)
);
-- NP-21: the same logical idempotency key returns the existing communication.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ncd_comm_idempotency ON ncd_communications (tenant_id, idempotency_key);
CREATE INDEX IF NOT EXISTS idx_ncd_comm_tenant_created ON ncd_communications (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ncd_comm_recipient ON ncd_communications (tenant_id, recipient_principal_id, created_at DESC);

-- ── Rendered content (§3.2, §9.1) ───────────────────────────────────────────
-- Exact as-issued content per channel. Immutable (INV-04, §9.2).
CREATE TABLE IF NOT EXISTS ncd_rendered_content (
    render_id               UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    communication_id        UUID         NOT NULL REFERENCES ncd_communications (communication_id),
    channel                 VARCHAR(20)  NOT NULL,
    template_version_id     UUID         NOT NULL REFERENCES ncd_templates (template_version_id),
    template_version        INTEGER      NOT NULL,
    locale                  VARCHAR(20)  NOT NULL,
    locale_fallback_from    VARCHAR(20),
    subject                 TEXT         NOT NULL,
    body                    TEXT         NOT NULL,
    subject_hash            VARCHAR(64)  NOT NULL,
    body_hash               VARCHAR(64)  NOT NULL,
    content_hash            VARCHAR(64)  NOT NULL,
    variable_hashes         JSONB        NOT NULL DEFAULT '{}'::jsonb,
    attachment_manifest     JSONB        NOT NULL DEFAULT '[]'::jsonb,
    rendered_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (communication_id, channel)
);

-- ── Provider bindings (§6.3) ────────────────────────────────────────────────
-- Platform configuration, not tenant data: which certified routes exist, what
-- evidence each can produce and where it processes. XIC is the eventual owner
-- of provider connections (§1.4); until it exists these rows are that
-- registry, and they hold a secret's ENVIRONMENT VARIABLE NAME, never the
-- secret (INV-26).
CREATE TABLE IF NOT EXISTS ncd_provider_bindings (
    binding_id              VARCHAR(64)  PRIMARY KEY,
    channel                 VARCHAR(20)  NOT NULL CHECK (channel IN ('EMAIL','IN_APP','SMS','PUSH')),
    provider_name           VARCHAR(64)  NOT NULL,
    regions                 TEXT[]       NOT NULL,
    evidence_capability     VARCHAR(2)   NOT NULL CHECK (evidence_capability IN ('E0','E1','E2','E3','E4')),
    supports_receipts       BOOLEAN      NOT NULL DEFAULT false,
    sender_identity         VARCHAR(255) NOT NULL,
    certified               BOOLEAN      NOT NULL DEFAULT false,
    failover_group          VARCHAR(64)  NOT NULL,
    priority                INTEGER      NOT NULL DEFAULT 100,
    cost_rank               INTEGER      NOT NULL DEFAULT 100,
    callback_secret_env     VARCHAR(128),
    status                  VARCHAR(20)  NOT NULL DEFAULT 'ACTIVE' CHECK (status IN ('ACTIVE','DISABLED')),
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ncd_binding_health (
    binding_id              VARCHAR(64)  PRIMARY KEY REFERENCES ncd_provider_bindings (binding_id),
    state                   VARCHAR(20)  NOT NULL DEFAULT 'HEALTHY' CHECK (state IN ('HEALTHY','CIRCUIT_OPEN')),
    reason                  TEXT,
    consecutive_failures    INTEGER      NOT NULL DEFAULT 0,
    changed_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    changed_by              VARCHAR(255) NOT NULL DEFAULT 'system'
);

-- The seed runs under the admin flag so the migration stays re-runnable once
-- the policies below exist (FORCE RLS applies to the table owner too).
SELECT set_config('app.binding_admin', 'true', false);

INSERT INTO ncd_provider_bindings (binding_id, channel, provider_name, regions, evidence_capability,
        supports_receipts, sender_identity, certified, failover_group, priority, cost_rank, callback_secret_env)
VALUES
    ('smtp-primary', 'EMAIL', 'smtp', ARRAY['GLOBAL'], 'E1', true, 'platform-transactional-mail', true, 'email', 10, 100, 'NCD_CALLBACK_SECRET_SMTP_PRIMARY'),
    ('in-app', 'IN_APP', 'in-app', ARRAY['GLOBAL'], 'E3', true, 'platform-inbox', true, 'in-app', 10, 0, NULL)
ON CONFLICT (binding_id) DO NOTHING;

INSERT INTO ncd_binding_health (binding_id) VALUES ('smtp-primary'), ('in-app')
ON CONFLICT (binding_id) DO NOTHING;

SELECT set_config('app.binding_admin', '', false);

-- ── Delivery jobs (§6.1) ────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ncd_delivery_jobs (
    job_id                  UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    communication_id        UUID         NOT NULL REFERENCES ncd_communications (communication_id),
    origin                  VARCHAR(20)  NOT NULL CHECK (origin IN ('INITIAL','RESEND','BULK')),
    resend_reason           TEXT,
    -- Ordered route plan: [{channel, binding_id, endpoint, endpoint_hash,
    -- endpoint_masked, provenance, evidence_capability, failover_group}]
    routes                  JSONB        NOT NULL,
    route_index             INTEGER      NOT NULL DEFAULT 0,
    attempts_on_route       INTEGER      NOT NULL DEFAULT 0,
    max_attempts_per_route  INTEGER      NOT NULL DEFAULT 3 CHECK (max_attempts_per_route >= 1),
    state                   VARCHAR(30)  NOT NULL DEFAULT 'QUEUED' CHECK (state IN (
                                'QUEUED','AWAITING_EVIDENCE','AWAITING_RESOLUTION',
                                'COMPLETED','EXCEPTION','EXPIRED','CANCELLED')),
    stream                  VARCHAR(20)  NOT NULL CHECK (stream IN ('CRITICAL','TRANSACTIONAL','OPERATIONAL','MARKETING')),
    priority                INTEGER      NOT NULL DEFAULT 0,
    next_run_at             TIMESTAMPTZ  NOT NULL DEFAULT now(),
    not_before              TIMESTAMPTZ,
    expires_at              TIMESTAMPTZ  NOT NULL,
    leased_until            TIMESTAMPTZ,
    last_deferral_reason    TEXT,
    exception_reason        TEXT,
    created_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    concluded_at            TIMESTAMPTZ,
    CONSTRAINT ncd_job_resend_has_reason CHECK ((origin = 'RESEND') = (resend_reason IS NOT NULL AND resend_reason <> ''))
);
CREATE INDEX IF NOT EXISTS idx_ncd_jobs_due ON ncd_delivery_jobs (priority DESC, next_run_at)
    WHERE state = 'QUEUED';
CREATE INDEX IF NOT EXISTS idx_ncd_jobs_comm ON ncd_delivery_jobs (tenant_id, communication_id);

-- ── Delivery attempts (§6.2) ────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ncd_attempts (
    attempt_id              UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    job_id                  UUID         NOT NULL REFERENCES ncd_delivery_jobs (job_id),
    communication_id        UUID         NOT NULL REFERENCES ncd_communications (communication_id),
    route_index             INTEGER      NOT NULL,
    channel                 VARCHAR(20)  NOT NULL,
    binding_id              VARCHAR(64)  NOT NULL REFERENCES ncd_provider_bindings (binding_id),
    intent_id               UUID         NOT NULL,
    intent_version          INTEGER      NOT NULL,
    purpose_class           VARCHAR(40)  NOT NULL,
    recipient_principal_id  VARCHAR(255) NOT NULL,
    origin                  VARCHAR(20)  NOT NULL CHECK (origin IN ('INITIAL','RETRY','FALLBACK','RESEND','BULK')),
    -- Sent to the provider (§6.1): unique to this attempt.
    idempotency_token       VARCHAR(80)  NOT NULL UNIQUE,
    content_hash            VARCHAR(64)  NOT NULL,
    render_id               UUID         NOT NULL REFERENCES ncd_rendered_content (render_id),
    -- The exact endpoint used, with provenance (§6.1, §9.2).
    recipient_snapshot      JSONB        NOT NULL,
    state                   VARCHAR(20)  NOT NULL DEFAULT 'CREATED' CHECK (state IN (
                                'CREATED','SUBMITTING','ACCEPTED','PENDING','UNKNOWN','DELIVERED','FAILED','BOUNCED')),
    provider_message_id     VARCHAR(255),
    failure_reason          TEXT,
    retryable               BOOLEAN      NOT NULL DEFAULT false,
    resolution_due_at       TIMESTAMPTZ,
    resolved_at             TIMESTAMPTZ,
    resolved_by_principal_id VARCHAR(255),
    resolution_note         TEXT,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    submitted_at            TIMESTAMPTZ,
    state_changed_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT ncd_attempt_unknown_has_due CHECK (state <> 'UNKNOWN' OR resolution_due_at IS NOT NULL),
    CONSTRAINT ncd_attempt_failed_has_reason CHECK (state NOT IN ('FAILED','BOUNCED') OR (failure_reason IS NOT NULL AND failure_reason <> ''))
);
CREATE INDEX IF NOT EXISTS idx_ncd_attempts_comm ON ncd_attempts (tenant_id, communication_id, created_at);
CREATE INDEX IF NOT EXISTS idx_ncd_attempts_rate ON ncd_attempts (tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_ncd_attempts_recipient_rate ON ncd_attempts (tenant_id, recipient_principal_id, channel, created_at);
CREATE INDEX IF NOT EXISTS idx_ncd_attempts_unknown ON ncd_attempts (resolution_due_at) WHERE state = 'UNKNOWN';
CREATE INDEX IF NOT EXISTS idx_ncd_attempts_awaiting ON ncd_attempts (submitted_at) WHERE state IN ('ACCEPTED','PENDING');
-- One ambiguous-or-in-flight attempt per job at a time.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ncd_attempts_one_open ON ncd_attempts (job_id)
    WHERE state IN ('CREATED','SUBMITTING','UNKNOWN');
-- §7.5: every provider message id maps to exactly one attempt within a
-- provider scope. A second attempt claiming the same id is refused here and
-- becomes a reconciliation exception (NP-27), never a silent merge.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ncd_attempts_provider_msg ON ncd_attempts (binding_id, provider_message_id)
    WHERE provider_message_id IS NOT NULL;

-- §6.2 as a database rule, not a convention: the transitions of the figure
-- and nothing else. UNKNOWN resolves only against the ORIGINAL attempt; no
-- state moves backwards (NP-25: a callback that overtook the API response
-- cannot be rolled back by it).
CREATE OR REPLACE FUNCTION ncd_attempt_transition() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'ncd_attempts rows are never deleted';
    END IF;
    IF NEW.attempt_id IS DISTINCT FROM OLD.attempt_id
        OR NEW.job_id IS DISTINCT FROM OLD.job_id
        OR NEW.communication_id IS DISTINCT FROM OLD.communication_id
        OR NEW.channel IS DISTINCT FROM OLD.channel
        OR NEW.binding_id IS DISTINCT FROM OLD.binding_id
        OR NEW.idempotency_token IS DISTINCT FROM OLD.idempotency_token
        OR NEW.content_hash IS DISTINCT FROM OLD.content_hash
        OR NEW.render_id IS DISTINCT FROM OLD.render_id
        OR NEW.recipient_snapshot IS DISTINCT FROM OLD.recipient_snapshot
        OR NEW.origin IS DISTINCT FROM OLD.origin
    THEN
        RAISE EXCEPTION 'attempt % identity, content and endpoint are immutable (§6.1)', OLD.attempt_id;
    END IF;
    IF OLD.provider_message_id IS NOT NULL AND NEW.provider_message_id IS DISTINCT FROM OLD.provider_message_id THEN
        RAISE EXCEPTION 'attempt % provider message id is immutable once known', OLD.attempt_id;
    END IF;
    IF NEW.state = OLD.state THEN
        RETURN NEW;
    END IF;
    IF NOT (
           (OLD.state = 'CREATED'    AND NEW.state IN ('SUBMITTING','FAILED'))
        OR (OLD.state = 'SUBMITTING' AND NEW.state IN ('ACCEPTED','PENDING','UNKNOWN','DELIVERED','FAILED','BOUNCED'))
        OR (OLD.state = 'ACCEPTED'   AND NEW.state IN ('PENDING','DELIVERED','FAILED','BOUNCED'))
        OR (OLD.state = 'PENDING'    AND NEW.state IN ('DELIVERED','FAILED','BOUNCED'))
        OR (OLD.state = 'UNKNOWN'    AND NEW.state IN ('ACCEPTED','DELIVERED','FAILED','BOUNCED'))
        -- A mailbox that accepted and later bounced: appended as correction
        -- evidence, and the attempt's state follows the later, stronger fact.
        OR (OLD.state = 'DELIVERED'  AND NEW.state = 'BOUNCED')
    ) THEN
        RAISE EXCEPTION 'attempt % cannot move from % to % (§6.2)', OLD.attempt_id, OLD.state, NEW.state;
    END IF;
    NEW.state_changed_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ncd_attempt_transition ON ncd_attempts;
CREATE TRIGGER trg_ncd_attempt_transition
    BEFORE UPDATE OR DELETE ON ncd_attempts
    FOR EACH ROW EXECUTE FUNCTION ncd_attempt_transition();

-- INV-12 / INV-13: "UNKNOWN never authorizes an immediate blind second
-- material send." While any attempt of a communication is UNKNOWN, no new
-- attempt of that communication may be created — by the worker, a fallback, a
-- retry or an operator resend. Enforced here so no code path can forget it.
CREATE OR REPLACE FUNCTION ncd_attempt_no_send_while_unknown() RETURNS TRIGGER AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM ncd_attempts
                WHERE communication_id = NEW.communication_id AND state = 'UNKNOWN') THEN
        RAISE EXCEPTION 'communication % has an UNKNOWN attempt; resolve it before another attempt (INV-13)', NEW.communication_id
            USING ERRCODE = 'P0014';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ncd_attempt_no_send_while_unknown ON ncd_attempts;
CREATE TRIGGER trg_ncd_attempt_no_send_while_unknown
    BEFORE INSERT ON ncd_attempts
    FOR EACH ROW EXECUTE FUNCTION ncd_attempt_no_send_while_unknown();

-- ── Stream controls (§7.4 circuit breakers) ─────────────────────────────────
CREATE TABLE IF NOT EXISTS ncd_stream_controls (
    tenant_id               VARCHAR(255) NOT NULL,
    stream                  VARCHAR(20)  NOT NULL CHECK (stream IN ('CRITICAL','TRANSACTIONAL','OPERATIONAL','MARKETING')),
    state                   VARCHAR(20)  NOT NULL CHECK (state IN ('ACTIVE','PAUSED')),
    reason                  TEXT         NOT NULL,
    automatic               BOOLEAN      NOT NULL DEFAULT false,
    changed_by_principal_id VARCHAR(255) NOT NULL,
    changed_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, stream)
);

-- ── Maker-checker approvals ─────────────────────────────────────────────────
-- One table for every "a second principal must agree" action: a resend of a
-- high-impact notice (§11.3), operator evidence on a regulated notice (NP-41),
-- reactivation of a hard-bounced/complaint endpoint (§7.3), a governed bulk
-- send (§6.5). The approver is whoever calls /approve — never a name in the
-- requester's body.
CREATE TABLE IF NOT EXISTS ncd_approvals (
    approval_id             UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    kind                    VARCHAR(30)  NOT NULL CHECK (kind IN ('RESEND','MANUAL_EVIDENCE','SUPPRESSION_LIFT','BULK_SEND')),
    target_id               VARCHAR(255) NOT NULL,
    payload                 JSONB        NOT NULL DEFAULT '{}'::jsonb,
    reason                  TEXT         NOT NULL CHECK (reason <> ''),
    status                  VARCHAR(20)  NOT NULL DEFAULT 'PENDING' CHECK (status IN ('PENDING','APPROVED','REJECTED')),
    requested_by_principal_id VARCHAR(255) NOT NULL,
    requested_at            TIMESTAMPTZ  NOT NULL DEFAULT now(),
    decided_by_principal_id VARCHAR(255),
    decided_at              TIMESTAMPTZ,
    decision_note           TEXT,
    CONSTRAINT ncd_approval_sod CHECK (decided_by_principal_id IS NULL OR decided_by_principal_id <> requested_by_principal_id),
    CONSTRAINT ncd_approval_decided CHECK ((status = 'PENDING') = (decided_at IS NULL))
);
CREATE INDEX IF NOT EXISTS idx_ncd_approvals_target ON ncd_approvals (tenant_id, kind, target_id);

-- ── Exceptions (the human path of §6.2 / §6.4 / §7.5) ────────────────────────
CREATE TABLE IF NOT EXISTS ncd_exceptions (
    exception_id            UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    communication_id        UUID,
    attempt_id              UUID,
    notice_id               UUID,
    kind                    VARCHAR(40)  NOT NULL CHECK (kind IN (
                                'UNKNOWN_UNRESOLVED','PROVIDER_ID_COLLISION','MISSING_CALLBACK',
                                'FALLBACK_BLOCKED','NO_ROUTE','DELIVERY_EXPIRED','DEADLINE_AT_RISK',
                                'ACK_EXPIRED','RECORD_HANDOFF_PENDING','STREAM_PAUSED','CALLBACK_REJECTED',
                                'ENDPOINT_REMEDIATION')),
    reason_code             VARCHAR(10),
    detail                  TEXT         NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    resolved_at             TIMESTAMPTZ,
    resolved_by_principal_id VARCHAR(255),
    resolution              TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_ncd_exceptions_open ON ncd_exceptions
    (tenant_id, kind, COALESCE(communication_id::text, ''), COALESCE(attempt_id::text, ''), COALESCE(notice_id::text, ''))
    WHERE resolved_at IS NULL;

-- ── Governed bulk sends (§6.5, NP-49/NP-50) ─────────────────────────────────
CREATE TABLE IF NOT EXISTS ncd_bulk_sends (
    bulk_id                 UUID         PRIMARY KEY,
    tenant_id               VARCHAR(255) NOT NULL,
    legal_entity_id         VARCHAR(255) NOT NULL,
    intent_id               UUID         NOT NULL,
    source_event_id         VARCHAR(255) NOT NULL,
    recipients              JSONB        NOT NULL,
    audience_count          INTEGER      NOT NULL CHECK (audience_count >= 1),
    -- sha256 of the sorted recipient set: dispatch must present the hash of
    -- the audience that was previewed and approved (NP-50).
    audience_hash           VARCHAR(64)  NOT NULL,
    variables               JSONB        NOT NULL DEFAULT '{}'::jsonb,
    locale                  VARCHAR(20)  NOT NULL,
    requires_approval       BOOLEAN      NOT NULL,
    approval_id             UUID REFERENCES ncd_approvals (approval_id),
    status                  VARCHAR(20)  NOT NULL DEFAULT 'PREVIEWED' CHECK (status IN ('PREVIEWED','DISPATCHED')),
    requested_by_principal_id VARCHAR(255) NOT NULL,
    created_at              TIMESTAMPTZ  NOT NULL DEFAULT now(),
    dispatched_at           TIMESTAMPTZ
);

-- ── RLS ─────────────────────────────────────────────────────────────────────
DO $$
DECLARE t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['ncd_communications','ncd_rendered_content','ncd_delivery_jobs','ncd_attempts',
                             'ncd_stream_controls','ncd_approvals','ncd_exceptions','ncd_bulk_sends']
    LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', t || '_tenant', t);
        EXECUTE format('CREATE POLICY %I ON %I FOR ALL
            USING (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), ''''))
            WITH CHECK (tenant_id = NULLIF(current_setting(''app.tenant_id'', true), ''''))', t || '_tenant', t);
    END LOOP;
END $$;

-- The worker discovers due work across tenants through the same SELECT-only
-- hatch the retry worker uses (app.platform_scope, migration 000004). It
-- projects ids and tenants only; every write that follows is tenant-scoped.
DROP POLICY IF EXISTS ncd_jobs_platform_read ON ncd_delivery_jobs;
CREATE POLICY ncd_jobs_platform_read ON ncd_delivery_jobs FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.platform_scope', true), ''), 'false') = 'true');
DROP POLICY IF EXISTS ncd_attempts_platform_read ON ncd_attempts;
CREATE POLICY ncd_attempts_platform_read ON ncd_attempts FOR SELECT
    USING (COALESCE(NULLIF(current_setting('app.platform_scope', true), ''), 'false') = 'true');

-- Bindings and their health are platform configuration: readable by every
-- tenant, writable only under the dedicated app.binding_admin flag.
ALTER TABLE ncd_provider_bindings ENABLE ROW LEVEL SECURITY;
ALTER TABLE ncd_provider_bindings FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ncd_bindings_read ON ncd_provider_bindings;
CREATE POLICY ncd_bindings_read ON ncd_provider_bindings FOR SELECT USING (true);
DROP POLICY IF EXISTS ncd_bindings_admin ON ncd_provider_bindings;
CREATE POLICY ncd_bindings_admin ON ncd_provider_bindings FOR ALL
    USING (COALESCE(NULLIF(current_setting('app.binding_admin', true), ''), 'false') = 'true')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.binding_admin', true), ''), 'false') = 'true');

ALTER TABLE ncd_binding_health ENABLE ROW LEVEL SECURITY;
ALTER TABLE ncd_binding_health FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ncd_binding_health_read ON ncd_binding_health;
CREATE POLICY ncd_binding_health_read ON ncd_binding_health FOR SELECT USING (true);
DROP POLICY IF EXISTS ncd_binding_health_admin ON ncd_binding_health;
CREATE POLICY ncd_binding_health_admin ON ncd_binding_health FOR ALL
    USING (COALESCE(NULLIF(current_setting('app.binding_admin', true), ''), 'false') = 'true')
    WITH CHECK (COALESCE(NULLIF(current_setting('app.binding_admin', true), ''), 'false') = 'true');
