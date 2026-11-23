-- Doc 03 §16 minimum conflict record: whether the claim is blocked, the
-- accountable owner, and the evidence that resolved the case.
ALTER TABLE public_ai.conflicts
    ADD COLUMN blocked_claim           BOOLEAN NOT NULL DEFAULT true,
    ADD COLUMN assigned_owner          TEXT    NOT NULL DEFAULT '',
    ADD COLUMN resolution_evidence_ref TEXT;
