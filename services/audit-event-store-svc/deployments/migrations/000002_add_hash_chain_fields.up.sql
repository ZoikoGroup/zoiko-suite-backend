-- Adds the hash-chain integrity fields required by the platform's original
-- data model spec (docs/original_doc/zoiko_suite_doc3.txt, rule 15.4:
-- "hash-chain integrity") — an audit review found these were missing from
-- the initial schema even though the doctrine treats them as a core,
-- queryable forensic-defensibility control, not something buried in the
-- payload JSONB.
--
-- correlation_id / causation_id: promoted to first-class columns so they can
-- be indexed and filtered on directly, instead of requiring a JSONB
-- extraction on every query. Nullable — not every event carries a
-- causation_id today, and pre-existing rows (if any) predate this column.
--
-- sequence_number: gives an unambiguous, gap-free total order for the hash
-- chain. stored_at alone is not safe for this — two events can share a
-- timestamp at typical clock resolution, and a hash chain built on an
-- ambiguous order is not tamper-evident.
--
-- Deliberately a plain BIGINT, NOT a BIGSERIAL/IDENTITY column. A Postgres
-- sequence's nextval() is consumed as soon as the DEFAULT expression is
-- evaluated for an INSERT — BEFORE the ON CONFLICT DO NOTHING check runs —
-- and is never rolled back on conflict. A duplicate event_id delivery would
-- therefore permanently burn a sequence number and leave a real gap in the
-- chain, defeating the "gap-free" property this column exists for. Instead,
-- the application computes the next sequence_number explicitly (MAX+1)
-- inside the same advisory-locked transaction as the insert — see
-- internal/store/store.go — so a deduped duplicate costs nothing.
--
-- payload_hash: SHA-256 of the exact payload bytes as stored, computed by
-- the application at insert time (see internal/store/store.go). Lets a
-- reviewer verify a row's payload was not altered after the fact by
-- recomputing the hash and comparing.
--
-- previous_event_hash: the payload_hash of the immediately preceding row in
-- the chain (by sequence_number), NULL only for the very first row ever
-- inserted. This is what makes the store tamper-EVIDENT rather than merely
-- tamper-resistant: altering or deleting any row breaks the hash link for
-- every row after it, which is externally verifiable without trusting this
-- service's own runtime.
--
-- Chain scope is deliberately GLOBAL (one chain across all tenants), not
-- per-tenant — a judgment call, documented here rather than silently
-- assumed. A global chain proves the store's entire insertion history is
-- intact in one verification pass; a per-tenant chain would need one
-- verification per tenant and would not catch cross-tenant reordering or
-- row insertion between tenants' chains.
--
-- Existing rows (if any, from before this migration) get NULL payload_hash
-- and previous_event_hash — the chain has a documented genesis point at the
-- first row inserted AFTER this migration runs, not before. Retroactively
-- hashing pre-existing rows is not attempted here since this project is
-- pre-production and had no real historical audit data to preserve.

ALTER TABLE audit_events
    ADD COLUMN IF NOT EXISTS correlation_id      TEXT,
    ADD COLUMN IF NOT EXISTS causation_id        TEXT,
    ADD COLUMN IF NOT EXISTS sequence_number     BIGINT,
    ADD COLUMN IF NOT EXISTS payload_hash        TEXT,
    ADD COLUMN IF NOT EXISTS previous_event_hash TEXT;

-- sequence_number must be unique to be a valid total order for the chain.
CREATE UNIQUE INDEX IF NOT EXISTS audit_events_sequence_number_idx
    ON audit_events (sequence_number);

-- Supports "fetch the most recent chain link" (ORDER BY sequence_number DESC
-- LIMIT 1), the query the store runs on every insert.
CREATE INDEX IF NOT EXISTS audit_events_sequence_number_desc_idx
    ON audit_events (sequence_number DESC);

-- Supports correlation_id-based lookups (e.g. "show me every audit event
-- for this request"), the primary reason to promote it out of the JSONB.
CREATE INDEX IF NOT EXISTS audit_events_correlation_id_idx
    ON audit_events (correlation_id) WHERE correlation_id IS NOT NULL;
