-- 000029_jurisdiction_imports.up.sql
--
-- REF-01 ImportJurisdictionUpdate (ZS-SVC-I-001 s4.11): "ISO/source updates
-- maker-checker in production", "Publisher/approver separation for externally
-- sourced code-list updates", "External source code+source version", "Source
-- standard/version, import hash, effective dates, mapping rationale".
--
-- An externally sourced update is not applied when it arrives. It is PROPOSED
-- as a batch of rows (add a jurisdiction, change one, retire one), every row is
-- validated and its errors kept, and a DIFFERENT principal approves it. Approval
-- applies the whole batch in one transaction or none of it: there is no partial
-- silent import. The batch and its rows stay as the evidence of what was
-- imported, from which source version, with which content hash, and who let it
-- through.
--
-- A (source standard, source version) is imported once: proposing the same
-- version with the same content again is a replay that returns the first
-- batch, and the same version with different content is refused. A REJECTED
-- batch frees the version, so a corrected file can be proposed again.

CREATE TABLE IF NOT EXISTS jurisdiction_import_batches (
    batch_id        UUID         PRIMARY KEY DEFAULT gen_random_uuid(),

    source_standard VARCHAR(128) NOT NULL,
    source_version  VARCHAR(64)  NOT NULL,
    -- SHA-256 over the normalised content, so "the same file" is decided by
    -- what is in it rather than by how it was sent.
    import_hash     CHAR(64)     NOT NULL,
    evidence_ref    TEXT         NOT NULL,
    summary         TEXT         NOT NULL,

    status          VARCHAR(16)  NOT NULL DEFAULT 'PROPOSED',
    item_count      INTEGER      NOT NULL,
    invalid_count   INTEGER      NOT NULL DEFAULT 0,

    proposed_by     TEXT         NOT NULL,
    proposed_at     TIMESTAMPTZ  NOT NULL DEFAULT clock_timestamp(),

    decided_by      TEXT,
    decided_at      TIMESTAMPTZ,
    decision_reason TEXT,

    -- What approval did, so reconciliation is a comparison of numbers.
    added_count     INTEGER,
    updated_count   INTEGER,
    retired_count   INTEGER,
    unchanged_count INTEGER,

    CONSTRAINT ck_jib_status  CHECK (status IN ('PROPOSED', 'APPROVED', 'REJECTED')),
    CONSTRAINT ck_jib_present CHECK (btrim(source_standard) <> '' AND btrim(source_version) <> ''
                                     AND btrim(evidence_ref) <> '' AND btrim(summary) <> ''),
    CONSTRAINT ck_jib_hash    CHECK (import_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT ck_jib_counts  CHECK (item_count > 0 AND invalid_count >= 0 AND invalid_count <= item_count),
    CONSTRAINT ck_jib_decision_shape CHECK (
        (status = 'PROPOSED' AND decided_by IS NULL AND decided_at IS NULL AND decision_reason IS NULL
            AND added_count IS NULL AND updated_count IS NULL AND retired_count IS NULL AND unchanged_count IS NULL)
        OR (status IN ('APPROVED', 'REJECTED') AND decided_by IS NOT NULL AND btrim(decided_by) <> '' AND decided_at IS NOT NULL)),
    CONSTRAINT ck_jib_reject_reason CHECK (status <> 'REJECTED' OR btrim(COALESCE(decision_reason, '')) <> ''),
    -- The approver is not the publisher. A proposer may withdraw (reject) their
    -- own batch, which is why the rule is on approval only.
    CONSTRAINT ck_jib_independent CHECK (status <> 'APPROVED' OR decided_by <> proposed_by),
    -- A batch with a bad row cannot be approved, and an approval says what it did.
    CONSTRAINT ck_jib_approved_clean CHECK (
        status <> 'APPROVED'
        OR (invalid_count = 0 AND added_count IS NOT NULL AND updated_count IS NOT NULL
            AND retired_count IS NOT NULL AND unchanged_count IS NOT NULL
            AND added_count + updated_count + retired_count + unchanged_count = item_count))
);

-- One live import per source version.
CREATE UNIQUE INDEX IF NOT EXISTS uq_jib_live_source_version
    ON jurisdiction_import_batches (source_standard, source_version)
    WHERE status IN ('PROPOSED', 'APPROVED');

CREATE INDEX IF NOT EXISTS idx_jib_status ON jurisdiction_import_batches (status, proposed_at DESC);

CREATE TABLE IF NOT EXISTS jurisdiction_import_items (
    item_id                 UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id                UUID         NOT NULL REFERENCES jurisdiction_import_batches (batch_id),
    item_no                 INTEGER      NOT NULL,
    action                  TEXT         NOT NULL,

    -- What the source row named. Targets and parents are named by code and type,
    -- or by id, and resolved when the batch is applied; they are kept as sent.
    target_ref_id           TEXT,
    target_code             TEXT,
    target_type             TEXT,
    jurisdiction_name       TEXT,
    authority_type          TEXT,
    parent_ref_id           TEXT,
    parent_code             TEXT,
    parent_type             TEXT,
    effective_from          TIMESTAMPTZ,
    successor_ref_id        TEXT,
    successor_code          TEXT,
    successor_type          TEXT,
    note                    TEXT,

    -- Per-record errors, kept. Empty means the row passed validation.
    validation_errors       JSONB        NOT NULL DEFAULT '[]'::jsonb,

    -- Set once, by approval: what the row did and to which jurisdiction.
    outcome                 VARCHAR(12),
    jurisdiction_id         UUID         REFERENCES jurisdictions (jurisdiction_id),

    CONSTRAINT uq_jii_item_no UNIQUE (batch_id, item_no),
    -- A row with an action the registry does not know is kept as sent, but only
    -- with the errors that say so, so it can never be approved.
    CONSTRAINT ck_jii_action  CHECK (action IN ('ADD', 'UPDATE', 'RETIRE') OR jsonb_array_length(validation_errors) > 0),
    CONSTRAINT ck_jii_item_no CHECK (item_no >= 1),
    CONSTRAINT ck_jii_outcome CHECK (outcome IS NULL OR outcome IN ('ADDED', 'UPDATED', 'RETIRED', 'UNCHANGED')),
    CONSTRAINT ck_jii_errors  CHECK (jsonb_typeof(validation_errors) = 'array'),
    CONSTRAINT ck_jii_outcome_target CHECK (outcome IS NULL OR jurisdiction_id IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_jii_jurisdiction ON jurisdiction_import_items (jurisdiction_id)
    WHERE jurisdiction_id IS NOT NULL;

-- A batch is evidence. Its content never changes; the only transition it sees is
-- the one decision, from PROPOSED, with the approval's counts.
CREATE OR REPLACE FUNCTION jurisdiction_import_batches_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'jurisdiction_import_batches is append-only (% refused)', TG_OP
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.status <> 'PROPOSED'
       OR NEW.batch_id        IS DISTINCT FROM OLD.batch_id
       OR NEW.source_standard IS DISTINCT FROM OLD.source_standard
       OR NEW.source_version  IS DISTINCT FROM OLD.source_version
       OR NEW.import_hash     IS DISTINCT FROM OLD.import_hash
       OR NEW.evidence_ref    IS DISTINCT FROM OLD.evidence_ref
       OR NEW.summary         IS DISTINCT FROM OLD.summary
       OR NEW.item_count      IS DISTINCT FROM OLD.item_count
       OR NEW.invalid_count   IS DISTINCT FROM OLD.invalid_count
       OR NEW.proposed_by     IS DISTINCT FROM OLD.proposed_by
       OR NEW.proposed_at     IS DISTINCT FROM OLD.proposed_at THEN
        RAISE EXCEPTION 'jurisdiction_import_batches is append-only: a proposed batch can only be decided, once'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_jurisdiction_import_batches_guard ON jurisdiction_import_batches;
CREATE TRIGGER trg_jurisdiction_import_batches_guard
    BEFORE UPDATE OR DELETE ON jurisdiction_import_batches
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_import_batches_guard();

DROP TRIGGER IF EXISTS trg_jurisdiction_import_batches_no_truncate ON jurisdiction_import_batches;
CREATE TRIGGER trg_jurisdiction_import_batches_no_truncate
    BEFORE TRUNCATE ON jurisdiction_import_batches
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdiction_import_batches_guard();

-- A row is evidence of what the source said. Only its outcome is filled in, once.
CREATE OR REPLACE FUNCTION jurisdiction_import_items_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'jurisdiction_import_items is append-only (% refused)', TG_OP
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF OLD.outcome IS NOT NULL
       OR NEW.item_id            IS DISTINCT FROM OLD.item_id
       OR NEW.batch_id           IS DISTINCT FROM OLD.batch_id
       OR NEW.item_no            IS DISTINCT FROM OLD.item_no
       OR NEW.action             IS DISTINCT FROM OLD.action
       OR NEW.target_ref_id      IS DISTINCT FROM OLD.target_ref_id
       OR NEW.target_code        IS DISTINCT FROM OLD.target_code
       OR NEW.target_type        IS DISTINCT FROM OLD.target_type
       OR NEW.jurisdiction_name  IS DISTINCT FROM OLD.jurisdiction_name
       OR NEW.authority_type     IS DISTINCT FROM OLD.authority_type
       OR NEW.parent_ref_id      IS DISTINCT FROM OLD.parent_ref_id
       OR NEW.parent_code        IS DISTINCT FROM OLD.parent_code
       OR NEW.parent_type        IS DISTINCT FROM OLD.parent_type
       OR NEW.effective_from     IS DISTINCT FROM OLD.effective_from
       OR NEW.successor_ref_id   IS DISTINCT FROM OLD.successor_ref_id
       OR NEW.successor_code     IS DISTINCT FROM OLD.successor_code
       OR NEW.successor_type     IS DISTINCT FROM OLD.successor_type
       OR NEW.note               IS DISTINCT FROM OLD.note
       OR NEW.validation_errors  IS DISTINCT FROM OLD.validation_errors THEN
        RAISE EXCEPTION 'jurisdiction_import_items is append-only: a row can only have its outcome recorded, once'
            USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_jurisdiction_import_items_guard ON jurisdiction_import_items;
CREATE TRIGGER trg_jurisdiction_import_items_guard
    BEFORE UPDATE OR DELETE ON jurisdiction_import_items
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_import_items_guard();

DROP TRIGGER IF EXISTS trg_jurisdiction_import_items_no_truncate ON jurisdiction_import_items;
CREATE TRIGGER trg_jurisdiction_import_items_no_truncate
    BEFORE TRUNCATE ON jurisdiction_import_items
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdiction_import_items_guard();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_jurisdiction_rules') THEN
        GRANT SELECT, INSERT, UPDATE ON jurisdiction_import_batches TO app_jurisdiction_rules;
        GRANT SELECT, INSERT, UPDATE ON jurisdiction_import_items   TO app_jurisdiction_rules;
    END IF;
END
$$;
