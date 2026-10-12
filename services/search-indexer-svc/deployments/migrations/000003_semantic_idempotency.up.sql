-- 000003 — semantic scopes, retrieval-evaluation certification, replay
-- protection, and the index behind ESR-018.
--
-- Every statement is idempotent. These files are mounted into Postgres's init
-- directory AND applied by hand from the RUNBOOK against a database that
-- already exists, so a second application must be a no-op rather than an
-- error halfway through.

-- ── §10.1 "Embedding model/version and preprocessing are pinned in the
--    IndexContract" ─────────────────────────────────────────────────────────
--
-- Nullable as a group: a lexical-only contract has none of them, a semantic
-- one has all of them. The CHECK makes "some of them" unrepresentable, because
-- a contract with a model but no dimensions is a contract that cannot build a
-- generation and would only fail later, at the least convenient moment.
ALTER TABLE index_contracts ADD COLUMN IF NOT EXISTS embedding_model          TEXT;
ALTER TABLE index_contracts ADD COLUMN IF NOT EXISTS embedding_model_version  TEXT;
ALTER TABLE index_contracts ADD COLUMN IF NOT EXISTS embedding_dimensions     INTEGER;
ALTER TABLE index_contracts ADD COLUMN IF NOT EXISTS embedding_source_fields  TEXT[];
ALTER TABLE index_contracts ADD COLUMN IF NOT EXISTS embedding_preprocessing  TEXT;
ALTER TABLE index_contracts ADD COLUMN IF NOT EXISTS embedding_similarity     TEXT;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'index_contracts_embedding_complete') THEN
        ALTER TABLE index_contracts ADD CONSTRAINT index_contracts_embedding_complete CHECK (
            (embedding_model IS NULL AND embedding_model_version IS NULL AND embedding_dimensions IS NULL
             AND embedding_source_fields IS NULL AND embedding_preprocessing IS NULL AND embedding_similarity IS NULL)
            OR
            (embedding_model IS NOT NULL AND embedding_model_version IS NOT NULL
             AND embedding_dimensions > 0 AND cardinality(embedding_source_fields) > 0
             AND embedding_preprocessing IS NOT NULL
             AND embedding_similarity IN ('cosinesimil', 'l2', 'innerproduct'))
        );
    END IF;
END $$;

-- ── §10.1 "Model migration requires parallel rebuild and retrieval-evaluation
--    certification before cutover" ──────────────────────────────────────────
--
-- Platform-scoped like generations: a certification is about one physical
-- index serving every tenant. No query text — cases_digest only (INV-17).
CREATE TABLE IF NOT EXISTS retrieval_evaluations (
    evaluation_id            UUID PRIMARY KEY,
    generation_id            UUID        NOT NULL REFERENCES index_generations(generation_id),
    scope_name               TEXT        NOT NULL,
    pinned_model             TEXT        NOT NULL,
    k                        INTEGER     NOT NULL CHECK (k > 0),
    cases                    INTEGER     NOT NULL CHECK (cases > 0),
    min_recall               DOUBLE PRECISION NOT NULL CHECK (min_recall > 0 AND min_recall <= 1),
    recall                   DOUBLE PRECISION NOT NULL CHECK (recall >= 0 AND recall <= 1),
    passed                   BOOLEAN     NOT NULL,
    cases_digest             TEXT        NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_principal_id  UUID        NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_retrieval_evaluations_generation
    ON retrieval_evaluations (generation_id, created_at DESC);

-- ── Idempotency-Key replay protection ────────────────────────────────────
--
-- The envelope contract has required Idempotency-Key on every material write
-- since d18abaa9; nothing read it, so a retried POST /v1/search-exports
-- recorded a second export authorization and a retried contract draft created
-- a second version. Same shape as identity-context-svc 000008, which is the
-- pattern the Group 1 audit names to copy: (tenant, endpoint, key) primary key,
-- a principal-bound request fingerprint, status 0 while in flight.
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id            UUID         NOT NULL,
    endpoint             TEXT         NOT NULL,
    idempotency_key      VARCHAR(255) NOT NULL,
    request_fingerprint  TEXT         NOT NULL,
    response_status      INTEGER      NOT NULL DEFAULT 0,
    response_body        JSONB        NOT NULL DEFAULT 'null'::jsonb,
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT idempotency_keys_pk PRIMARY KEY (tenant_id, endpoint, idempotency_key),
    CONSTRAINT idempotency_keys_status_sane CHECK (response_status = 0 OR response_status BETWEEN 100 AND 599)
);
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created_at ON idempotency_keys (created_at);

-- Tenant-scoped absolutely: a stored response is a response a tenant received.
-- The platform hatch is for the retention purge only.
ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_policies WHERE tablename = 'idempotency_keys'
                   AND policyname = 'tenant_isolation_policy') THEN
        CREATE POLICY tenant_isolation_policy ON idempotency_keys
            FOR ALL
            USING (
                tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                OR search_indexer_platform_scope()
            )
            WITH CHECK (
                tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                OR search_indexer_platform_scope()
            );
    END IF;
END $$;

-- ── ESR-018 / NP-59 ──────────────────────────────────────────────────────
--
-- Every governed search now asks "does this tenant have a FAILED restriction
-- in this scope?" before it runs. Partial, because FAILED is the rare state
-- and the question is asked on the hot path.
CREATE INDEX IF NOT EXISTS idx_restriction_tombstones_failed
    ON restriction_tombstones (tenant_id, scope_name)
    WHERE state = 'FAILED';
