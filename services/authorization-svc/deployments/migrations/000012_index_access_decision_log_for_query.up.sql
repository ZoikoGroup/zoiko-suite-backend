-- Migration: 000012_index_access_decision_log_for_query.up.sql
--
-- Makes access_decision_log READABLE as an audit trail, rather than only as a
-- point lookup by an id the reader has to already possess.
--
-- ── WHY ─────────────────────────────────────────────────────────────────────
--
-- Doc 03 §8.3 gives this service two evidence obligations:
--
--     every decision logged with actor, action, basis, and outcome
--     denials must be evidentially retrievable
--
-- The first has held since 000001. The second has not, and the gap was not in
-- the schema — it was that the only read path was FindAccessDecisionByID.
-- Retrieval by primary key is retrieval only for somebody who already has the
-- key, and a denial's key exists in exactly one place: the response returned to
-- the service that was refused. So auditing a denial meant reading the calling
-- service's logs to find a UUID to hand back to this one. The console's own
-- lookup box said so in its hint text.
--
-- GET /v1/access-decisions (store.ListAccessDecisions) is the read that closes
-- it, and this migration is the index that makes it affordable on the platform's
-- largest table.
--
-- ── THE INDEX, AND WHY IT REPLACES ONE RATHER THAN JOINING IT ───────────────
--
-- 000009 created idx_access_decision_log_tenant (tenant_id, decided_at DESC).
-- Every audit read is tenant-scoped and ordered by decided_at DESC, so that
-- index already serves the unfiltered listing. What it does not serve is the
-- query the obligation actually names: DENIALS. decision_outcome is 'GRANTED'
-- for the overwhelming majority of rows — /v1/authorize is called on nearly
-- every mutating request platform-wide and grants most of them — so filtering
-- on DENIED through the tenant index means an index scan over predominantly
-- non-matching rows, then a filter, at exactly the moment somebody is trying to
-- answer an auditor.
--
-- (tenant_id, decision_outcome, decided_at DESC) serves both: the outcome
-- filter by equality in the middle column, and the unfiltered tenant listing by
-- the leading prefix. The old index is therefore a strict prefix of the new one
-- and is DROPPED rather than kept alongside it. That keeps the index count on
-- this table at four.
--
-- Index count matters more here than on any other table on the platform. This
-- is the hottest write in the estate — one INSERT per authorization evaluation,
-- on the path 111 services call before every material action — and every index
-- is maintained on every one of those inserts. Adding a fifth index to buy an
-- audit query would have charged the whole platform's write latency for a read
-- that runs when an auditor asks. Replacing a prefix costs nothing and is why
-- this migration is a swap.
--
-- ── WHAT IS DELIBERATELY NOT INDEXED ────────────────────────────────────────
--
-- principal_id and legal_entity_id already have their own (col, decided_at
-- DESC) indexes from 000009, so a listing filtered by either is served. There
-- is deliberately NO index on action_type or decision_basis: both are filters
-- applied AFTER tenant and date have already narrowed the scan to one month of
-- one tenant, which is small, and neither is a leading predicate in any query
-- the handler builds. An index per filterable column is how a hot write path
-- dies of read convenience.
--
-- Created on the parent, so it propagates to every existing partition and to
-- every partition create_access_decision_log_partition() makes later — a new
-- month is never briefly unindexed. Same mechanism 000009 relies on.

BEGIN;

CREATE INDEX idx_access_decision_log_tenant_outcome
    ON access_decision_log (tenant_id, decision_outcome, decided_at DESC);

COMMENT ON INDEX idx_access_decision_log_tenant_outcome IS
    'Serves GET /v1/access-decisions: tenant-scoped listings ordered by decided_at DESC, with or without a decision_outcome filter. Replaces idx_access_decision_log_tenant, of which it is a superset.';

DROP INDEX idx_access_decision_log_tenant;

COMMIT;
