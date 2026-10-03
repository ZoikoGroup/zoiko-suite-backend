-- Schema for assistant-rag-svc (AI-03, ZS-SVC-N-001 §4/§13 Wave 7)
-- Migration: 000001_initial_schema.up.sql

-- Source Grants: seller-managed registration that a source is
-- authorized for retrieval under a given purpose. Presence is
-- authorization — Retrieve excludes any unregistered candidate before
-- it can ever reach a RetrievalSet.
CREATE TABLE IF NOT EXISTS source_grants (
    tenant_id       VARCHAR(64)  NOT NULL,
    source_ref      VARCHAR(256) NOT NULL,
    purpose         VARCHAR(128) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by      VARCHAR(128) NOT NULL,
    PRIMARY KEY (tenant_id, source_ref, purpose)
);

-- Tool Policies: seller-managed tool allow-list. ProposeToolCall
-- refuses any tool absent here; protected is frozen onto each proposal
-- at creation time from this row.
CREATE TABLE IF NOT EXISTS tool_policies (
    tenant_id       VARCHAR(64)  NOT NULL,
    tool_name       VARCHAR(128) NOT NULL,
    target_domain   VARCHAR(128) NOT NULL,
    protected       BOOLEAN      NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by      VARCHAR(128) NOT NULL,
    PRIMARY KEY (tenant_id, tool_name)
);

-- Assistant Sessions: forward-only lifecycle, Active -> exactly one of
-- Completed/Blocked/Escalated, then terminal.
CREATE TABLE IF NOT EXISTS assistant_sessions (
    session_id   TEXT         PRIMARY KEY
        CHECK (session_id ~ '^ask_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id    VARCHAR(64)  NOT NULL,
    subject_id   VARCHAR(128) NOT NULL,
    purpose      VARCHAR(128) NOT NULL,
    status       VARCHAR(16)  NOT NULL DEFAULT 'Active'
        CHECK (status IN ('Active','Completed','Blocked','Escalated')),
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by   VARCHAR(128) NOT NULL,
    ended_at     TIMESTAMPTZ,
    ended_by     VARCHAR(128)
);

-- Retrieval Sets: one per Retrieve call, immutable once written.
CREATE TABLE IF NOT EXISTS retrieval_sets (
    retrieval_set_id   TEXT         PRIMARY KEY
        CHECK (retrieval_set_id ~ '^rts_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id          VARCHAR(64)  NOT NULL,
    session_id         TEXT         NOT NULL REFERENCES assistant_sessions(session_id),
    query              TEXT         NOT NULL,
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by         VARCHAR(128) NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_retrieval_sets_session ON retrieval_sets(tenant_id, session_id);

-- Retrieved Items: one row per candidate source evaluated against
-- source_grants. Unauthorized items are kept for audit but can never
-- be cited. Immutable once written.
CREATE TABLE IF NOT EXISTS retrieved_items (
    item_id           TEXT         PRIMARY KEY
        CHECK (item_id ~ '^rti_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id         VARCHAR(64)  NOT NULL,
    retrieval_set_id  TEXT         NOT NULL REFERENCES retrieval_sets(retrieval_set_id),
    source_ref        VARCHAR(256) NOT NULL,
    snippet           TEXT         NOT NULL DEFAULT '',
    authorized        BOOLEAN      NOT NULL,
    excluded_reason   VARCHAR(128),
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CHECK (authorized OR excluded_reason IS NOT NULL),
    UNIQUE (retrieval_set_id, source_ref)
);

-- Prompt Executions: one per Ask call, immutable provenance. A
-- prompt/model upgrade is always a new execution.
CREATE TABLE IF NOT EXISTS prompt_executions (
    execution_id       TEXT         PRIMARY KEY
        CHECK (execution_id ~ '^pex_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id          VARCHAR(64)  NOT NULL,
    session_id         TEXT         NOT NULL REFERENCES assistant_sessions(session_id),
    retrieval_set_id   TEXT         NOT NULL REFERENCES retrieval_sets(retrieval_set_id),
    model_provider     VARCHAR(128) NOT NULL,
    model_version      VARCHAR(128) NOT NULL,
    prompt_version     VARCHAR(128) NOT NULL DEFAULT '',
    query              TEXT         NOT NULL,
    answer_text        TEXT         NOT NULL,
    content_hash       CHAR(64)     NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    created_at         TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_prompt_executions_session ON prompt_executions(tenant_id, session_id);

-- Citations: immutable, each tied to exactly one authorized
-- RetrievedItem within the execution's own retrieval set. The FK to
-- retrieved_items plus the application-level authorized check together
-- make an unauthorized citation impossible to persist.
CREATE TABLE IF NOT EXISTS citations (
    citation_id    TEXT         PRIMARY KEY
        CHECK (citation_id ~ '^cit_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id      VARCHAR(64)  NOT NULL,
    execution_id   TEXT         NOT NULL REFERENCES prompt_executions(execution_id),
    source_ref     VARCHAR(256) NOT NULL,
    snippet        TEXT         NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_citations_execution ON citations(tenant_id, execution_id);

-- AI Response Evidence: append-only, exactly one row per execution.
CREATE TABLE IF NOT EXISTS ai_response_evidence (
    evidence_id      TEXT         PRIMARY KEY
        CHECK (evidence_id ~ '^are_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id        VARCHAR(64)  NOT NULL,
    execution_id     TEXT         NOT NULL UNIQUE REFERENCES prompt_executions(execution_id),
    outcome          VARCHAR(16)  NOT NULL CHECK (outcome IN ('Answered')),
    citation_count   INT          NOT NULL CHECK (citation_count >= 1),
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

-- Tool Proposals: forward-only, Proposed -> (Approved|Rejected),
-- Approved -> Executed. target_domain/protected are frozen at creation
-- from tool_policies.
CREATE TABLE IF NOT EXISTS tool_proposals (
    proposal_id            TEXT         PRIMARY KEY
        CHECK (proposal_id ~ '^tpr_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id              VARCHAR(64)  NOT NULL,
    session_id             TEXT         NOT NULL REFERENCES assistant_sessions(session_id),
    tool_name              VARCHAR(128) NOT NULL,
    target_domain          VARCHAR(128) NOT NULL,
    protected              BOOLEAN      NOT NULL,
    justification          TEXT         NOT NULL,
    justification_source   VARCHAR(32)  NOT NULL CHECK (justification_source IN ('System','RetrievedContent')),
    status                 VARCHAR(16)  NOT NULL DEFAULT 'Proposed'
        CHECK (status IN ('Proposed','Approved','Rejected','Executed')),
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by             VARCHAR(128) NOT NULL,
    decided_at             TIMESTAMPTZ,
    decided_by             VARCHAR(128),
    decision_reason        TEXT
);

CREATE INDEX IF NOT EXISTS idx_tool_proposals_session ON tool_proposals(tenant_id, session_id);

-- Idempotency Keys: scoped per tenant from the start.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id       VARCHAR(64)  NOT NULL,
    owner_scope     VARCHAR(32)  NOT NULL,
    principal_id    VARCHAR(128) NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    operation       VARCHAR(64)  NOT NULL,
    request_sha256  VARCHAR(64)  NOT NULL,
    resource_id     VARCHAR(128) NOT NULL,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id   UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type    VARCHAR(64)  NOT NULL,
    aggregate_id      VARCHAR(128) NOT NULL,
    event_type        VARCHAR(64)  NOT NULL,
    payload           JSONB        NOT NULL,
    tenant_id         VARCHAR(64),
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at      TIMESTAMPTZ,
    publish_attempts  INT          NOT NULL DEFAULT 0,
    last_error        TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE source_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE source_grants FORCE ROW LEVEL SECURITY;
CREATE POLICY source_grants_tenant_isolation ON source_grants
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE tool_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE tool_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY tool_policies_tenant_isolation ON tool_policies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE assistant_sessions ENABLE ROW LEVEL SECURITY;
ALTER TABLE assistant_sessions FORCE ROW LEVEL SECURITY;
CREATE POLICY assistant_sessions_tenant_isolation ON assistant_sessions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE retrieval_sets ENABLE ROW LEVEL SECURITY;
ALTER TABLE retrieval_sets FORCE ROW LEVEL SECURITY;
CREATE POLICY retrieval_sets_tenant_isolation ON retrieval_sets
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE retrieved_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE retrieved_items FORCE ROW LEVEL SECURITY;
CREATE POLICY retrieved_items_tenant_isolation ON retrieved_items
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE prompt_executions ENABLE ROW LEVEL SECURITY;
ALTER TABLE prompt_executions FORCE ROW LEVEL SECURITY;
CREATE POLICY prompt_executions_tenant_isolation ON prompt_executions
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE citations ENABLE ROW LEVEL SECURITY;
ALTER TABLE citations FORCE ROW LEVEL SECURITY;
CREATE POLICY citations_tenant_isolation ON citations
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE ai_response_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_response_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY ai_response_evidence_tenant_isolation ON ai_response_evidence
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE tool_proposals ENABLE ROW LEVEL SECURITY;
ALTER TABLE tool_proposals FORCE ROW LEVEL SECURITY;
CREATE POLICY tool_proposals_tenant_isolation ON tool_proposals
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- ── Lifecycle triggers ───────────────────────────────────────────────────────

-- Source Grants / Tool Policies: mutable reference data, no DELETE —
-- stays visible as governance history, superseded only by a later
-- UPSERT-style write.
CREATE OR REPLACE FUNCTION reject_delete()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows cannot be deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_source_grants_no_delete
    BEFORE DELETE ON source_grants
    FOR EACH ROW EXECUTE FUNCTION reject_delete();

CREATE TRIGGER trigger_tool_policies_no_delete
    BEFORE DELETE ON tool_policies
    FOR EACH ROW EXECUTE FUNCTION reject_delete();

-- Assistant Sessions: forward-only, Active -> exactly one terminal
-- state, then fully immutable.
CREATE OR REPLACE FUNCTION enforce_session_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'assistant sessions cannot be deleted';
    END IF;
    IF OLD.status IN ('Completed', 'Blocked', 'Escalated') THEN
        RAISE EXCEPTION 'assistant session % is % and immutable', OLD.session_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status AND NEW.status NOT IN ('Completed', 'Blocked', 'Escalated') THEN
        RAISE EXCEPTION 'invalid assistant session transition from Active to %', NEW.status;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.subject_id IS DISTINCT FROM OLD.subject_id
        OR NEW.purpose IS DISTINCT FROM OLD.purpose
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
    THEN
        RAISE EXCEPTION 'assistant session % may only have its status/ended fields change', OLD.session_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_session_lifecycle
    BEFORE UPDATE OR DELETE ON assistant_sessions
    FOR EACH ROW EXECUTE FUNCTION enforce_session_lifecycle();

-- Retrieval Sets / Retrieved Items / Prompt Executions / Citations /
-- AI Response Evidence: fully immutable once written.
CREATE OR REPLACE FUNCTION reject_immutable_row()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable once written', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_retrieval_sets_immutable
    BEFORE UPDATE OR DELETE ON retrieval_sets
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_retrieved_items_immutable
    BEFORE UPDATE OR DELETE ON retrieved_items
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_prompt_executions_immutable
    BEFORE UPDATE OR DELETE ON prompt_executions
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_citations_immutable
    BEFORE UPDATE OR DELETE ON citations
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

CREATE TRIGGER trigger_ai_response_evidence_immutable
    BEFORE UPDATE OR DELETE ON ai_response_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_row();

-- Tool Proposals: forward-only, Proposed -> (Approved|Rejected),
-- Approved -> Executed, then fully terminal.
CREATE OR REPLACE FUNCTION enforce_tool_proposal_lifecycle()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'tool proposals cannot be deleted';
    END IF;
    IF OLD.status IN ('Rejected', 'Executed') THEN
        RAISE EXCEPTION 'tool proposal % is % and immutable', OLD.proposal_id, OLD.status;
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Proposed' THEN
                IF NEW.status NOT IN ('Approved', 'Rejected') THEN
                    RAISE EXCEPTION 'invalid tool proposal transition from Proposed to %', NEW.status;
                END IF;
            WHEN 'Approved' THEN
                IF NEW.status <> 'Executed' THEN
                    RAISE EXCEPTION 'invalid tool proposal transition from Approved to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown tool proposal status %', OLD.status;
        END CASE;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.session_id IS DISTINCT FROM OLD.session_id
        OR NEW.tool_name IS DISTINCT FROM OLD.tool_name
        OR NEW.target_domain IS DISTINCT FROM OLD.target_domain
        OR NEW.protected IS DISTINCT FROM OLD.protected
        OR NEW.justification IS DISTINCT FROM OLD.justification
        OR NEW.justification_source IS DISTINCT FROM OLD.justification_source
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
    THEN
        RAISE EXCEPTION 'tool proposal % may only have its status/decision fields change', OLD.proposal_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_tool_proposal_lifecycle
    BEFORE UPDATE OR DELETE ON tool_proposals
    FOR EACH ROW EXECUTE FUNCTION enforce_tool_proposal_lifecycle();
