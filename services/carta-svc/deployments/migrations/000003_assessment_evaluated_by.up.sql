-- evaluated_by: the verified principal (X-Principal-Id) that asked for the
-- evaluation, when one was presented. NULL means the caller presented no
-- principal — today that is gateway-auth-svc scoring a request during
-- authentication, which sends X-Tenant-ID only (the subject being scored is
-- subject_id, not the caller). Recorded as NULL rather than guessed.
--
-- Adding a nullable column does not touch existing rows, so the append-only
-- trigger from 000001 is not involved.
ALTER TABLE carta_assessments ADD COLUMN IF NOT EXISTS evaluated_by VARCHAR(255)
    CHECK (evaluated_by IS NULL OR evaluated_by <> '');
