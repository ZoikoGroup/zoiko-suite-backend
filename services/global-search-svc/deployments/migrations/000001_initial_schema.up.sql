-- Schema for global-search-svc (DATA-06, ZS-SVC-N-001 §4)
-- Migration: 000001_initial_schema.up.sql

-- Search Indexes: one named, tenant-scoped index scope. Unlike every
-- other DATA-0x service this session, this table is genuinely mutable —
-- an index legitimately cycles Indexing -> Available -> Rebuilding ->
-- Available forever. It is not evidence; it is a disposable projection.
CREATE TABLE IF NOT EXISTS search_indexes (
    index_id             TEXT         PRIMARY KEY
        CHECK (index_id ~ '^dsi_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id              VARCHAR(64)  NOT NULL,
    scope                    VARCHAR(128) NOT NULL,
    status                     VARCHAR(16)  NOT NULL DEFAULT 'Indexing' CHECK (status IN ('Indexing', 'Available', 'Rebuilding')),
    created_at                  TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                    VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, scope)
);

-- Search Policies: seller-managed reference config (mutable in place via
-- upsert, same idiom as commercial_currencies elsewhere in this
-- platform) — governs bounded staleness per scope.
CREATE TABLE IF NOT EXISTS search_policies (
    policy_id             TEXT         PRIMARY KEY
        CHECK (policy_id ~ '^dsp_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                VARCHAR(64)  NOT NULL,
    scope                      VARCHAR(128) NOT NULL,
    max_staleness_seconds         BIGINT       NOT NULL CHECK (max_staleness_seconds > 0),
    created_at                      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    created_by                        VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, scope)
);

-- Index Documents: one indexed object, a disposable derived projection —
-- mutable via upsert (re-indexing updates in place) and hard-deletable
-- via PurgeIndexObject (deletion/hold propagation requires REAL removal,
-- not a soft flag that leaves content queryable). Restricted +
-- allowed_principal_ids is the actual authorization-before-retrieval
-- enforcement point: every read query filters on this in its WHERE
-- clause, never by fetching everything and filtering in application code
-- afterward.
CREATE TABLE IF NOT EXISTS index_documents (
    doc_id                   TEXT         PRIMARY KEY
        CHECK (doc_id ~ '^did_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
    tenant_id                  VARCHAR(64)  NOT NULL,
    index_id                     TEXT         NOT NULL REFERENCES search_indexes(index_id),
    scope                          VARCHAR(128) NOT NULL,
    object_type                      VARCHAR(64)  NOT NULL,
    object_ref                         VARCHAR(256) NOT NULL,
    content_text                         TEXT         NOT NULL,
    content_tsv                            tsvector     GENERATED ALWAYS AS (to_tsvector('english', content_text)) STORED,
    content_hash                             CHAR(64)     NOT NULL CHECK (content_hash ~ '^[0-9a-f]{64}$'),
    classification                             VARCHAR(64)  NOT NULL,
    residency_region                             VARCHAR(64)  NOT NULL,
    restricted                                     BOOLEAN      NOT NULL DEFAULT FALSE,
    allowed_principal_ids                            TEXT[],
    source_updated_at                                  TIMESTAMPTZ  NOT NULL,
    indexed_at                                           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    indexed_by                                             VARCHAR(128) NOT NULL,
    UNIQUE (tenant_id, index_id, object_type, object_ref),
    CHECK (NOT restricted OR allowed_principal_ids IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_index_documents_tsv ON index_documents USING GIN (content_tsv);
CREATE INDEX IF NOT EXISTS idx_index_documents_scope ON index_documents(tenant_id, scope);
CREATE INDEX IF NOT EXISTS idx_index_documents_source_updated ON index_documents(tenant_id, scope, source_updated_at);

-- Idempotency Keys: scoped per tenant from the start.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(64)  NOT NULL,
    owner_scope           VARCHAR(32)  NOT NULL,
    principal_id             VARCHAR(128) NOT NULL,
    idempotency_key            VARCHAR(128) NOT NULL,
    operation                    VARCHAR(64)  NOT NULL,
    request_sha256                 VARCHAR(64)  NOT NULL,
    resource_id                      VARCHAR(128) NOT NULL,
    created_at                         TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, owner_scope, principal_id, idempotency_key)
);

-- Outbox Events: transactional outbox for event emission.
CREATE TABLE IF NOT EXISTS outbox_events (
    outbox_event_id     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type        VARCHAR(64)  NOT NULL,
    aggregate_id            VARCHAR(128) NOT NULL,
    event_type                VARCHAR(64)  NOT NULL,
    payload                      JSONB        NOT NULL,
    tenant_id                      VARCHAR(64),
    created_at                       TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    published_at                       TIMESTAMPTZ,
    publish_attempts                     INT          NOT NULL DEFAULT 0,
    last_error                             TEXT
);

CREATE INDEX IF NOT EXISTS idx_outbox_events_unpublished ON outbox_events(published_at) WHERE published_at IS NULL;

-- ── Row Level Security ──────────────────────────────────────────────────────

ALTER TABLE search_indexes ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_indexes FORCE ROW LEVEL SECURITY;
CREATE POLICY search_indexes_tenant_isolation ON search_indexes
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE search_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE search_policies FORCE ROW LEVEL SECURITY;
CREATE POLICY search_policies_tenant_isolation ON search_policies
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE index_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE index_documents FORCE ROW LEVEL SECURITY;
CREATE POLICY index_documents_tenant_isolation ON index_documents
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
CREATE POLICY idempotency_keys_tenant_isolation ON idempotency_keys
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;
CREATE POLICY outbox_events_tenant_isolation ON outbox_events
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true));

-- ── Lifecycle ────────────────────────────────────────────────────────────────

-- Search Indexes: legal transitions only. No terminal state — a search
-- index cycles for the life of the scope. A same-status "touch" (no
-- status column change) is always allowed since nothing else on this row
-- needs protecting.
CREATE OR REPLACE FUNCTION enforce_search_index_transitions()
RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'search index cannot be deleted; purge its documents instead';
    END IF;
    IF OLD.status <> NEW.status THEN
        CASE OLD.status
            WHEN 'Indexing' THEN
                IF NEW.status <> 'Available' THEN
                    RAISE EXCEPTION 'invalid search index transition from Indexing to %', NEW.status;
                END IF;
            WHEN 'Available' THEN
                IF NEW.status <> 'Rebuilding' THEN
                    RAISE EXCEPTION 'invalid search index transition from Available to %', NEW.status;
                END IF;
            WHEN 'Rebuilding' THEN
                IF NEW.status <> 'Available' THEN
                    RAISE EXCEPTION 'invalid search index transition from Rebuilding to %', NEW.status;
                END IF;
            ELSE
                RAISE EXCEPTION 'unknown search index status %', OLD.status;
        END CASE;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trigger_search_index_transitions
    BEFORE UPDATE OR DELETE ON search_indexes
    FOR EACH ROW EXECUTE FUNCTION enforce_search_index_transitions();
