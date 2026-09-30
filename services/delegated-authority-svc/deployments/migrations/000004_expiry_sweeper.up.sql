-- Let a background sweeper expire delegations in every tenant, and record the
-- moment an authority actually ended rather than the moment somebody noticed.
--
-- WHAT WAS WRONG
--
-- Expiry was lazy and piggybacked on reads: ListDelegations, GetDelegation and
-- RevokeDelegation each called ExpireDue for the tenant of the request that was
-- passing through. Two consequences, and the second is the serious one.
--
--   1. expired_at was set to now() -- the instant of the sweep. For a grant
--      whose window closed on Friday at 17:00 and whose register was next read
--      on Monday at 09:00, the row asserted the authority ended Monday morning.
--      Doc 04 §6.3 requires that "access decisions are evidence and must not be
--      ephemeral"; evidence that misdates the end of an authority by a weekend
--      is worse than absent, because it is confidently wrong and nothing marks
--      it as approximate. authority.expired carried the same wrong timestamp to
--      every consumer.
--
--   2. ExpireDue is tenant-scoped, so only the tenant doing the reading was
--      ever swept. A tenant whose register nobody opens never expires anything
--      at all: its grants stay ACTIVE past their window and authority.expired
--      is never published, so identity-context-svc is never told to end the
--      delegate's session. The one tenant least likely to be watched is the one
--      whose lapsed authority persists longest.
--
-- expired_at is now written as effective_to (when the authority ended, which is
-- a property of the grant and knowable in advance) while updated_at keeps now()
-- (when this service observed it). Those are different facts and the schema now
-- holds both instead of conflating them.
--
-- WHY A GUC RATHER THAN A SUPERUSER SWEEP
--
-- The sweeper is not a request: it crosses every tenant from one background
-- loop, exactly as the outbox relay does, so it is admitted the same way --
-- by installing a named capability GUC that the policy names. The alternative,
-- running it as a role that bypasses RLS, would give the sweep unlimited reach
-- rather than this one documented exemption, and would make the exemption
-- invisible in the schema.
--
-- NULLIF for the same reason 000003 gives: Postgres keeps a custom GUC in the
-- SESSION after a transaction-local SET is reset, with '' as its value, so on a
-- pooled connection that has already served a request these read as the empty
-- string and not NULL.

-- ── delegation_grants: admit the sweeper ─────────────────────────────────────
--
-- Also picks up the NULLIF treatment 000003 applied to the outbox. Both columns
-- are VARCHAR so comparing '' is harmless today -- it matches nothing and fails
-- closed. It is written this way so the policy stays correct if tenant_id ever
-- becomes a UUID, where the same expression raises 22P02 and turns a refusal
-- into a 500.
DROP POLICY IF EXISTS tenant_isolation_policy ON delegation_grants;
CREATE POLICY tenant_isolation_policy ON delegation_grants FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.expiry_sweeper', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.expiry_sweeper', true), ''), 'false') = 'true'
    );

-- ── delegation_outbox: the sweep enqueues authority.expired ──────────────────
--
-- The sweeper writes its events in the same transaction as the flip, so it has
-- to be admitted here too. Without this the sweep would fail at the INSERT and
-- roll back the flip -- which fails safe, but silently and forever.
DROP POLICY IF EXISTS outbox_tenant_isolation ON delegation_outbox;
CREATE POLICY outbox_tenant_isolation ON delegation_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.expiry_sweeper', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.expiry_sweeper', true), ''), 'false') = 'true'
    );

-- ── The index the sweeper runs on ────────────────────────────────────────────
--
-- 000001 indexed (tenant_id, effective_to) WHERE status = 'ACTIVE', which suits
-- the per-tenant read-path sweep. The background sweep asks the cross-tenant
-- question -- "anything due anywhere?" -- and would fall back to a sequential
-- scan of the whole register on every tick.
CREATE INDEX IF NOT EXISTS idx_delegation_grants_active_effective_to
    ON delegation_grants (effective_to) WHERE status = 'ACTIVE';

-- ── Backfill: correct the rows the old sweep misdated ────────────────────────
--
-- Every EXPIRED row was stamped at observation time, so each one currently
-- overstates how long its authority lasted. The true value was always present
-- in the same row, as effective_to, which is why this is a correction and not
-- a fabrication: nothing is invented, one column is replaced by another column
-- of the same row that was always the authoritative answer.
--
-- Guarded on expired_at > effective_to so it only touches rows that are
-- actually wrong, and so re-running the migration is a no-op.
UPDATE delegation_grants
   SET expired_at = effective_to
 WHERE status = 'EXPIRED'
   AND expired_at IS NOT NULL
   AND expired_at > effective_to;
