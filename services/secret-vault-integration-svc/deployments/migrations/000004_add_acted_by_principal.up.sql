-- Record WHO performed each audited act, not only whose access it concerned.
--
-- secret_access_audit_log.requested_by_principal_id names the SUBJECT of the
-- record: the principal whose access to the material is being requested,
-- granted or denied. For REQUESTED, GRANTED and DENIED that is also the actor,
-- so one column answered both questions and the distinction never surfaced.
--
-- It stops holding for the two administrative events:
--
--   REVOKED  — written with the LEASE HOLDER's id, because that is what the
--              lease row carries. The principal who actually revoked it was
--              read from X-Principal-Id, authorized against SECRET_LEASE_REVOKE,
--              and then discarded. Nothing in the four tables recorded it:
--              outcome_detail was left empty on this path and correlation_id is
--              a trace handle, not an identity. An auditor asking "who killed
--              this lease?" — the first question asked after an incident — had
--              no answer available at all.
--   ROTATED  — written with rotated_by_principal_id, which IS the actor. That
--              row was therefore correct by accident while meaning something
--              different from every other row in the same column.
--
-- Rather than overload requested_by_principal_id further (which would break
-- idx_secret_access_audit_log_principal's meaning, and the "show me everything
-- about principal X's access" query that index exists for), the actor gets its
-- own column. Every event type populates it, so one predicate answers "what did
-- this principal DO" across all five, and the subject column keeps meaning only
-- what it always meant.
--
-- Nullable, with no backfill: rows written before this migration genuinely do
-- not carry the actor, and inventing one — copying requested_by_principal_id
-- across, which is right for three event types and wrong for REVOKED, the exact
-- case this column exists for — would manufacture evidence. NULL reads as
-- "recorded before this service captured the actor", which is true. The table
-- is append-only, so this backfills itself as new rows arrive.
ALTER TABLE secret_access_audit_log
    ADD COLUMN acted_by_principal_id TEXT;

COMMENT ON COLUMN secret_access_audit_log.acted_by_principal_id IS
    'The principal that performed this act (the authenticated caller). Distinct '
    'from requested_by_principal_id, which names the subject whose access the '
    'record concerns; the two differ for REVOKED. NULL on rows written before '
    'migration 000004.';

-- "Everything this principal did", the counterpart to the existing
-- idx_secret_access_audit_log_principal ("everything done TO this principal").
-- Partial: pre-migration rows are all NULL and none of them can match.
CREATE INDEX idx_secret_access_audit_log_actor
    ON secret_access_audit_log (acted_by_principal_id)
    WHERE acted_by_principal_id IS NOT NULL;
