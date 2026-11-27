-- ZS-SVC-Z-001 DQC-02 / INV-11 / INV-12 / TC-07 / TC-08 / NP-16 (docs/architecture/DQC_GAP_MATRIX.md, step E).
--
-- WHAT WAS WRONG with releasing an import item from quarantine:
--
--   1. The evidence was destroyed. A retry set quarantine_reason, quarantine_owner
--      and quarantined_by back to NULL on the item row, and item-level quarantine
--      actions wrote nothing durable to the batch log (only transient outbox
--      events). Once an item was retried, nothing recorded who had quarantined it,
--      why, or who put it back. INV-11: "Quarantine never deletes original evidence."
--   2. No distinct authority and no independence. Quarantining and releasing used
--      one permission, and the same person could do both (NP-16).
--   3. No correction evidence. The retry reason was free text, so nothing said what
--      had been corrected in the owning domain (INV-12: release cannot bypass
--      authoritative domain correction and re-validation).
--   4. No re-validation record (TC-08) and no quarantine state (NONE / PARTIAL /
--      FULL / RELEASE_PENDING).
--
-- THE FIX is a ledger: one append-only row per quarantine, release request,
-- decision and re-validation, per item. The item row keeps doing what it did (it
-- is the item's current status); the ledger is the durable history that survives
-- the item moving on. The database, not just the application, enforces the rules
-- that make a release governed.

CREATE TABLE IF NOT EXISTS import_quarantine_ledger (
    ledger_id            BIGSERIAL    PRIMARY KEY,
    tenant_id            VARCHAR(64)  NOT NULL,
    batch_id             VARCHAR(64)  NOT NULL,
    row_no               INTEGER      NOT NULL,
    kind                 VARCHAR(24)  NOT NULL CHECK (kind IN
        ('QUARANTINED', 'RELEASE_REQUESTED', 'RELEASE_REFUSED', 'RELEASED', 'REVALIDATED')),
    actor                VARCHAR(255) NOT NULL CHECK (btrim(actor) <> ''),
    reason               TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    -- QUARANTINED: who owns resolving it.
    owner                VARCHAR(255),
    -- RELEASE_REQUESTED: the correction made in the owning domain that justifies re-sending the item.
    correction_ref       VARCHAR(500),
    -- the ledger row this one answers: a request answers nothing; RELEASED/RELEASE_REFUSED answer a
    -- request; REVALIDATED answers the RELEASED row it checks.
    answers_ledger_id    BIGINT       REFERENCES import_quarantine_ledger (ledger_id),
    -- REVALIDATED: what the owning domain concluded when it re-validated the released item.
    revalidation_outcome VARCHAR(16),
    correlation_id       VARCHAR(255) NOT NULL DEFAULT '',
    occurred_at          TIMESTAMPTZ  NOT NULL DEFAULT now(),
    FOREIGN KEY (batch_id, row_no) REFERENCES import_items (batch_id, row_no),
    CONSTRAINT import_quarantine_ledger_shape CHECK (
        (kind = 'QUARANTINED'
            AND owner IS NOT NULL AND btrim(owner) <> ''
            AND correction_ref IS NULL AND answers_ledger_id IS NULL AND revalidation_outcome IS NULL)
     OR (kind = 'RELEASE_REQUESTED'
            AND correction_ref IS NOT NULL AND btrim(correction_ref) <> ''
            AND owner IS NULL AND answers_ledger_id IS NULL AND revalidation_outcome IS NULL)
     OR (kind IN ('RELEASED', 'RELEASE_REFUSED')
            AND answers_ledger_id IS NOT NULL
            AND owner IS NULL AND correction_ref IS NULL AND revalidation_outcome IS NULL)
     OR (kind = 'REVALIDATED'
            AND answers_ledger_id IS NOT NULL AND revalidation_outcome IN ('IMPORTED', 'QUARANTINED', 'FAILED')
            AND owner IS NULL AND correction_ref IS NULL)
    )
);
CREATE INDEX IF NOT EXISTS idx_import_quarantine_ledger_item ON import_quarantine_ledger (batch_id, row_no, ledger_id);
CREATE INDEX IF NOT EXISTS idx_import_quarantine_ledger_tenant ON import_quarantine_ledger (tenant_id, batch_id);

-- How many times an item has been released from quarantine. It selects the key the item is delivered
-- under (imports.DeliveryKey): a release is a new attempt after a correction, and must not replay the
-- refusal an owning domain cached under the original key. Only a release increments it.
ALTER TABLE import_items
    ADD COLUMN IF NOT EXISTS release_generation INTEGER NOT NULL DEFAULT 0 CHECK (release_generation >= 0);

-- A row can be answered once: a request is released OR refused, never both; a release is
-- re-validated once. (The guard trigger gives the clear message; this is the backstop.)
CREATE UNIQUE INDEX IF NOT EXISTS uq_import_quarantine_ledger_one_answer
    ON import_quarantine_ledger (answers_ledger_id) WHERE answers_ledger_id IS NOT NULL;

-- Items that are quarantined at the moment this runs have only their own columns as evidence.
-- Give each a ledger row built from them, so every quarantined item has an episode to release
-- and none is left without a record. Done before the guard trigger and row-level security are
-- attached, because it describes history, it does not make a decision.
INSERT INTO import_quarantine_ledger (tenant_id, batch_id, row_no, kind, actor, reason, owner, occurred_at)
SELECT i.tenant_id, i.batch_id, i.row_no, 'QUARANTINED',
       COALESCE(NULLIF(btrim(i.quarantined_by), ''), 'system:import-executor'),
       COALESCE(NULLIF(btrim(i.quarantine_reason), ''), '(no reason was recorded before the quarantine ledger existed)'),
       COALESCE(NULLIF(btrim(i.quarantine_owner), ''), 'unassigned'),
       i.updated_at
  FROM import_items i
 WHERE i.status = 'QUARANTINED'
   AND NOT EXISTS (SELECT 1 FROM import_quarantine_ledger l WHERE l.batch_id = i.batch_id AND l.row_no = i.row_no);

-- ── The rules that make a release governed, enforced by the database ────────

CREATE OR REPLACE FUNCTION import_quarantine_ledger_guard() RETURNS TRIGGER AS $$
DECLARE
    item_status VARCHAR(20);
    ref         RECORD;
    quarantiner VARCHAR(255);
BEGIN
    IF NEW.kind = 'RELEASE_REQUESTED' THEN
        SELECT status INTO item_status FROM import_items WHERE batch_id = NEW.batch_id AND row_no = NEW.row_no;
        IF item_status IS DISTINCT FROM 'QUARANTINED' THEN
            RAISE EXCEPTION 'only a quarantined item can have its release requested' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (
            SELECT 1 FROM import_quarantine_ledger r
             WHERE r.batch_id = NEW.batch_id AND r.row_no = NEW.row_no AND r.kind = 'RELEASE_REQUESTED'
               AND NOT EXISTS (SELECT 1 FROM import_quarantine_ledger c
                                WHERE c.answers_ledger_id = r.ledger_id AND c.kind IN ('RELEASED', 'RELEASE_REFUSED'))
        ) THEN
            RAISE EXCEPTION 'a release request for this item is already open' USING ERRCODE = 'integrity_constraint_violation';
        END IF;

    ELSIF NEW.kind IN ('RELEASED', 'RELEASE_REFUSED') THEN
        SELECT * INTO ref FROM import_quarantine_ledger WHERE ledger_id = NEW.answers_ledger_id;
        IF NOT FOUND OR ref.kind <> 'RELEASE_REQUESTED' OR ref.batch_id <> NEW.batch_id OR ref.row_no <> NEW.row_no THEN
            RAISE EXCEPTION 'a release decision must answer a release request for the same item' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF EXISTS (SELECT 1 FROM import_quarantine_ledger c WHERE c.answers_ledger_id = ref.ledger_id) THEN
            RAISE EXCEPTION 'that release request has already been answered' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        IF NEW.kind = 'RELEASED' THEN
            -- Independence (NP-16): nobody releases what they asked to release, or what they quarantined.
            IF NEW.actor = ref.actor THEN
                RAISE EXCEPTION 'the person who requested a release cannot approve it' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
            SELECT q.actor INTO quarantiner FROM import_quarantine_ledger q
             WHERE q.batch_id = NEW.batch_id AND q.row_no = NEW.row_no AND q.kind = 'QUARANTINED' AND q.ledger_id < ref.ledger_id
             ORDER BY q.ledger_id DESC LIMIT 1;
            IF quarantiner IS NOT NULL AND NEW.actor = quarantiner THEN
                RAISE EXCEPTION 'the person who quarantined an item cannot approve its release' USING ERRCODE = 'integrity_constraint_violation';
            END IF;
        END IF;

    ELSIF NEW.kind = 'REVALIDATED' THEN
        SELECT * INTO ref FROM import_quarantine_ledger WHERE ledger_id = NEW.answers_ledger_id;
        IF NOT FOUND OR ref.kind <> 'RELEASED' OR ref.batch_id <> NEW.batch_id OR ref.row_no <> NEW.row_no THEN
            RAISE EXCEPTION 're-validation must answer a release of the same item' USING ERRCODE = 'integrity_constraint_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_import_quarantine_ledger_guard ON import_quarantine_ledger;
CREATE TRIGGER trg_import_quarantine_ledger_guard BEFORE INSERT ON import_quarantine_ledger
    FOR EACH ROW EXECUTE FUNCTION import_quarantine_ledger_guard();

-- The ledger is evidence: it is only ever appended to.
CREATE OR REPLACE FUNCTION import_quarantine_ledger_immutable() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'import_quarantine_ledger is append-only' USING ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_import_quarantine_ledger_immutable ON import_quarantine_ledger;
CREATE TRIGGER trg_import_quarantine_ledger_immutable BEFORE UPDATE OR DELETE ON import_quarantine_ledger
    FOR EACH ROW EXECUTE FUNCTION import_quarantine_ledger_immutable();

-- ── row-level security, like every other import table ───────────────────────
ALTER TABLE import_quarantine_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE import_quarantine_ledger FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS import_quarantine_ledger_tenant ON import_quarantine_ledger;
CREATE POLICY import_quarantine_ledger_tenant ON import_quarantine_ledger FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), ''));
