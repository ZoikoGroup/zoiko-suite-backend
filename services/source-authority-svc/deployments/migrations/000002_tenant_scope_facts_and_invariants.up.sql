-- 000002_tenant_scope_facts_and_invariants.up.sql
--
-- Two tables, two different answers, and 000001 gave them the same one.
--
-- source_authority_maps is genuinely platform-wide reference data. "ADP outranks
-- the HR spreadsheet for PAYROLL_GROSS_PAY" is a statement about which connected
-- SYSTEM is trusted, not about anyone's payroll. It stays untenanted, which is
-- also what keeps internal/events/publisher.go's envelope comment true.
--
-- normalized_facts is not reference data and never was. A row there is one
-- business fact about one business entity -- an employee's gross pay, a
-- counterparty's billing contact -- carried in a JSONB fact_value, keyed by a
-- free-text entity_ref that no registry constrains. 000001 gave that table no
-- tenant column, so every tenant's facts sat in one undivided pool, and
-- GET /v1/source-authority/resolve returned raw fact_value out of it. The
-- envelope middleware defaults to write-strict, which admits reads with no
-- envelope at all, and Resolve is deliberately ungated on authorization, so that
-- read required no tenant, no principal and no grant: a guessed entity_ref was
-- the whole of the access control.
--
-- This migration tenant-scopes the facts and leaves the maps alone.

-- ── normalized_facts: the tenant boundary ────────────────────────────────────

-- DEFAULT '' then DROP DEFAULT, deliberately.
--
-- A pre-existing row cannot be attributed to a tenant after the fact: nothing in
-- it records which tenant reported it, and entity_ref is free text, so there is
-- nothing to infer from. Guessing would be worse than leaving it out -- it would
-- hand one tenant another's fact under the appearance of a fix. Any row already
-- in this table therefore keeps '' and, because every query below and the RLS
-- policy both filter on an equality with a non-empty tenant, becomes visible to
-- no tenant at all. That is the intended outcome: unattributable data is
-- withheld, not redistributed. Dropping the default afterwards means no NEW row
-- can inherit it.
ALTER TABLE normalized_facts ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT '';
ALTER TABLE normalized_facts ALTER COLUMN tenant_id DROP DEFAULT;

ALTER TABLE normalized_facts
    ADD CONSTRAINT normalized_facts_tenant_present
    CHECK (tenant_id <> '') NOT VALID;

-- FORCE, not just ENABLE. Postgres exempts a table's owner from row-level
-- security unless the table is FORCE, and these services connect as the owner --
-- the exact defect delegated-authority-svc's 000002 was written to correct, so
-- this one does not repeat it. Isolation does not rest on the policy either:
-- pg_store.go carries an explicit tenant_id predicate on every statement.
ALTER TABLE normalized_facts ENABLE ROW LEVEL SECURITY;
ALTER TABLE normalized_facts FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation_policy ON normalized_facts;
CREATE POLICY tenant_isolation_policy ON normalized_facts FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- ── normalized_facts: replay protection ──────────────────────────────────────

-- correlation_id was in RecordFactRequest from the start and the handler never
-- read it. On an append-only fact log that is not a cosmetic omission: a
-- retried POST -- the client never saw the first response, the pod restarted
-- mid-write -- appends a SECOND row for the same observation. Two rows from the
-- same source_system at the same effective_at then race in the resolver's
-- DISTINCT ON, and a fact log whose whole purpose is to be an exact account of
-- what each source said acquires an observation that no source ever made.
ALTER TABLE normalized_facts ADD COLUMN IF NOT EXISTS correlation_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_normalized_facts_idempotency
    ON normalized_facts (tenant_id, correlation_id)
    WHERE correlation_id IS NOT NULL;

-- The resolver reads latest-per-source within one tenant; tenant_id leads.
DROP INDEX IF EXISTS idx_normalized_facts_lookup;
CREATE INDEX IF NOT EXISTS idx_normalized_facts_tenant_lookup
    ON normalized_facts (tenant_id, field_family, entity_ref, source_system, effective_at DESC);

-- authority_class is documented as AUTHORITATIVE | DERIVED | CACHED and was
-- enforced nowhere -- not in Go, not here. A typo stored a fact in a class no
-- reader knows, and since the column carries a DEFAULT, a misspelled JSON key
-- stored it silently under the default instead.
ALTER TABLE normalized_facts
    ADD CONSTRAINT normalized_facts_authority_class_known
    CHECK (authority_class IN ('AUTHORITATIVE', 'DERIVED', 'CACHED')) NOT VALID;

-- ── source_authority_maps: supersession ──────────────────────────────────────

-- effective_to has existed since 000001, the resolver has always honoured it,
-- and nothing has ever been able to set it. The documented way to change a
-- precedence rule -- "a changed precedence is a new row, never an UPDATE" --
-- therefore left BOTH rows currently effective, and the resolver's join matched
-- both. Demoting a source from rank 1 to rank 3 produced two ranked entries for
-- that one source and the resolver took the better of them, so the demotion had
-- no effect and reported none.
--
-- POST /v1/source-authority-maps/{id}/supersede sets this column. It is the only
-- UPDATE this service performs, and it end-dates a rule rather than rewriting
-- it: precedence_rank, conflict_route and the rest stay exactly as recorded, so
-- a resolution made last week can still be explained by the row that made it.
ALTER TABLE source_authority_maps
    ADD CONSTRAINT source_authority_maps_window_ordered
    CHECK (effective_to IS NULL OR effective_to > effective_from) NOT VALID;

ALTER TABLE source_authority_maps
    ADD CONSTRAINT source_authority_maps_rank_positive
    CHECK (precedence_rank > 0) NOT VALID;

-- Same omission as the facts: correlation_id was in the request type and read
-- by nothing. Here the consequence is the mirror image -- the unique index on
-- (field_family, source_system, effective_from) turns a retry into a 409, so a
-- client that never saw the first response is told its rule conflicts with
-- itself.
ALTER TABLE source_authority_maps ADD COLUMN IF NOT EXISTS correlation_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_source_authority_maps_idempotency
    ON source_authority_maps (correlation_id)
    WHERE correlation_id IS NOT NULL;

-- Supersession is an audited act; the row records who ended the rule and when.
ALTER TABLE source_authority_maps ADD COLUMN IF NOT EXISTS superseded_at TIMESTAMPTZ;
ALTER TABLE source_authority_maps ADD COLUMN IF NOT EXISTS superseded_by_principal_id TEXT;

ALTER TABLE source_authority_maps
    ADD CONSTRAINT source_authority_maps_supersession_has_evidence
    CHECK ((superseded_at IS NULL) = (superseded_by_principal_id IS NULL)) NOT VALID;

-- The resolver picks one map row per (field_family, source_system) -- the most
-- recently effective one -- rather than joining every row that matches.
CREATE INDEX IF NOT EXISTS idx_source_authority_maps_effective
    ON source_authority_maps (field_family, source_system, effective_from DESC);
