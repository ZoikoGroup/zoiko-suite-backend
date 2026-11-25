-- Who recorded the execution. signed_by is signing EVIDENCE (the signatory
-- named on the executed instrument, caller-asserted until LEG-08 exists) and
-- may legitimately be someone other than the caller, e.g. a counterparty
-- signatory. The authenticated principal (X-Principal-Id) that recorded the
-- execution was not stored anywhere; it is now.
ALTER TABLE contracts ADD COLUMN IF NOT EXISTS execution_recorded_by TEXT;
