-- Doc 06 §21, Doc 09 §05/§21: the control audit is hash-chained, so a
-- deleted, reordered or rewritten row is detectable even by someone who can
-- bypass the append-only trigger. row_hash = sha256(prev_hash | fields),
-- computed by the service (store.AuditRowHash) under a transaction-scoped
-- advisory lock so appends are serialised. Rows written before this
-- migration keep empty hashes; the chain starts at the first hashed row.
-- request_id and actor_role complete the Doc 09 audit envelope.
ALTER TABLE public_ai.control_audit
    ADD COLUMN request_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN actor_role TEXT NOT NULL DEFAULT '',
    ADD COLUMN prev_hash  TEXT NOT NULL DEFAULT '',
    ADD COLUMN row_hash   TEXT NOT NULL DEFAULT '';
