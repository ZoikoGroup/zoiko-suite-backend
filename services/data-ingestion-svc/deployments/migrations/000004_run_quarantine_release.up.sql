-- ZS-SVC-Z-001 DQC-02 / INV-11 / INV-12 / NP-16 (docs/architecture/DQC_GAP_MATRIX.md, step H): governed release for
-- RUN-LEVEL quarantine (INT-04).
--
-- A run-level quarantine item is a whole batch that was refused (for example a schema-incompatible batch). The service
-- keeps only a hash of its payload (payload_ref), never the records, so a quarantined batch cannot be "re-queued": the
-- way back is a corrected re-submission through CommitBatch. Until now nothing tied that re-submission to the
-- quarantine, and the identical bad payload could simply be committed again by anyone who may commit.
--
--   quarantine_item_release_ledger   append-only. REQUESTED (names the correction) -> REFUSED | APPROVED (an independent
--                                    decision) -> CONSUMED (the corrected batch that landed under the approval).
--
-- Rules held here as well as in the service: one open request per item; a decision answers an open request and is
-- never made by its requester; an approval is never made by whoever quarantined the batch; an approval is consumed
-- once, by a landing; and nothing is requested for an item that has already been released.
--
-- uuid ids, not a sequence, so the least-privilege application role needs no sequence grant.

CREATE TABLE IF NOT EXISTS quarantine_item_release_ledger (
    ledger_id          UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          VARCHAR(64)  NOT NULL,
    quarantine_id      TEXT         NOT NULL REFERENCES quarantine_items (quarantine_id),
    kind               VARCHAR(16)  NOT NULL CHECK (kind IN ('REQUESTED', 'REFUSED', 'APPROVED', 'CONSUMED')),
    actor              VARCHAR(255) NOT NULL CHECK (btrim(actor) <> ''),
    reason             TEXT         NOT NULL CHECK (btrim(reason) <> ''),
    -- REQUESTED: the correction made in the owning domain that justifies re-submitting the batch.
    correction_ref     VARCHAR(500),
    -- the entry this one answers: REFUSED/APPROVED answer a REQUESTED, CONSUMED answers an APPROVED.
    answers_ledger_id  UUID         REFERENCES quarantine_item_release_ledger (ledger_id),
    -- CONSUMED: the landing object the corrected batch became.
    landing_id         TEXT,
    occurred_at        TIMESTAMPTZ  NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT quarantine_release_shape CHECK (
        (kind = 'REQUESTED' AND btrim(coalesce(correction_ref, '')) <> '' AND answers_ledger_id IS NULL AND landing_id IS NULL)
     OR (kind IN ('REFUSED', 'APPROVED') AND answers_ledger_id IS NOT NULL AND correction_ref IS NULL AND landing_id IS NULL)
     OR (kind = 'CONSUMED' AND answers_ledger_id IS NOT NULL AND btrim(coalesce(landing_id, '')) <> '' AND correction_ref IS NULL)
    )
);

CREATE INDEX IF NOT EXISTS idx_quarantine_release_item ON quarantine_item_release_ledger (quarantine_id, occurred_at);
-- An entry is answered at most once.
CREATE UNIQUE INDEX IF NOT EXISTS uq_quarantine_release_answered ON quarantine_item_release_ledger (answers_ledger_id)
    WHERE answers_ledger_id IS NOT NULL;

CREATE OR REPLACE FUNCTION enforce_quarantine_release_append_only()
RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'the quarantine release ledger is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_quarantine_release_append_only ON quarantine_item_release_ledger;
CREATE TRIGGER trigger_quarantine_release_append_only BEFORE UPDATE OR DELETE ON quarantine_item_release_ledger
    FOR EACH ROW EXECUTE FUNCTION enforce_quarantine_release_append_only();

CREATE OR REPLACE FUNCTION enforce_quarantine_release_guard()
RETURNS trigger AS $$
DECLARE
    answered quarantine_item_release_ledger%ROWTYPE;
    quarantiner TEXT;
BEGIN
    IF NEW.kind = 'REQUESTED' THEN
        IF EXISTS (SELECT 1 FROM quarantine_item_release_ledger WHERE quarantine_id = NEW.quarantine_id AND kind = 'CONSUMED') THEN
            RAISE EXCEPTION 'this quarantined batch has already been released';
        END IF;
        IF EXISTS (SELECT 1 FROM quarantine_item_release_ledger q
                    WHERE q.quarantine_id = NEW.quarantine_id AND q.kind IN ('REQUESTED', 'APPROVED')
                      AND NOT EXISTS (SELECT 1 FROM quarantine_item_release_ledger a WHERE a.answers_ledger_id = q.ledger_id
                                         AND (q.kind = 'REQUESTED' OR a.kind = 'CONSUMED'))) THEN
            RAISE EXCEPTION 'a release request or an unused approval is already open for this quarantined batch';
        END IF;
        RETURN NEW;
    END IF;

    SELECT * INTO answered FROM quarantine_item_release_ledger WHERE ledger_id = NEW.answers_ledger_id;
    IF answered.quarantine_id IS DISTINCT FROM NEW.quarantine_id THEN
        RAISE EXCEPTION 'an entry can only answer an entry of the same quarantined batch';
    END IF;
    IF NEW.kind IN ('REFUSED', 'APPROVED') THEN
        IF answered.kind <> 'REQUESTED' THEN
            RAISE EXCEPTION 'a decision answers a release request';
        END IF;
        IF NEW.actor = answered.actor THEN
            RAISE EXCEPTION 'a release request cannot be decided by the person who made it';
        END IF;
        IF NEW.kind = 'APPROVED' THEN
            SELECT quarantined_by INTO quarantiner FROM quarantine_items WHERE quarantine_id = NEW.quarantine_id;
            IF NEW.actor = quarantiner THEN
                RAISE EXCEPTION 'a release cannot be approved by the person who quarantined the batch';
            END IF;
        END IF;
    ELSIF NEW.kind = 'CONSUMED' THEN
        IF answered.kind <> 'APPROVED' THEN
            RAISE EXCEPTION 'only an approval can be consumed';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trigger_quarantine_release_guard ON quarantine_item_release_ledger;
CREATE TRIGGER trigger_quarantine_release_guard BEFORE INSERT ON quarantine_item_release_ledger
    FOR EACH ROW EXECUTE FUNCTION enforce_quarantine_release_guard();

ALTER TABLE quarantine_item_release_ledger ENABLE ROW LEVEL SECURITY;
ALTER TABLE quarantine_item_release_ledger FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS quarantine_release_tenant_isolation ON quarantine_item_release_ledger;
CREATE POLICY quarantine_release_tenant_isolation ON quarantine_item_release_ledger
    FOR ALL USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));
