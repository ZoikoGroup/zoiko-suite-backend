-- Closes ORG-10's own documented dependency ("AP-10 fingerprints active
-- version"): this column records the payee-banking-identity-svc (ORG-10)
-- active destination pinned for this payee at RequestPaymentAuthorization
-- time, so verifyStillEligible can later detect a real destination change
-- (supersession/suspension) the same way it already detects a supplier
-- profile identity change. Empty when the payee has no ORG-10 coverage
-- yet — a real, current gap (ORG-10 is new), not a fabricated absence.
ALTER TABLE authorization_payee_snapshots
    ADD COLUMN destination_id TEXT NOT NULL DEFAULT '';
