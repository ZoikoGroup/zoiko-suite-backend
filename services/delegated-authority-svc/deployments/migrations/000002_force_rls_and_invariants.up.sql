-- FORCE, not just ENABLE.
--
-- 000001 enabled row-level security and wrote a tenant isolation policy, and
-- that policy has never applied to a single query this service makes: Postgres
-- exempts a table's OWNER from row-level security unless the table is declared
-- FORCE ROW LEVEL SECURITY, and these services connect as the owner.
--
-- Isolation here does not rest on the policy -- pg_store.go carries an explicit
-- `tenant_id = $1` predicate on every statement, including the expiry sweep --
-- but a policy that silently does nothing is worse than no policy, because it
-- reads as a control that is present. On a register of who may act for whom,
-- that is exactly the control an auditor would point at.
ALTER TABLE delegation_grants FORCE ROW LEVEL SECURITY;

-- The USING expression doubles as the WITH CHECK for INSERT/UPDATE when no
-- WITH CHECK is given. Stated explicitly now that it is load-bearing: under
-- FORCE, a row may only be written into the tenant the connection installed.
DROP POLICY IF EXISTS tenant_isolation_policy ON delegation_grants;
CREATE POLICY tenant_isolation_policy ON delegation_grants FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- Every CHECK below is added NOT VALID, deliberately.
--
-- NOT VALID enforces the constraint on every INSERT and UPDATE from here on;
-- what it skips is the scan that would reject the table outright if an EXISTING
-- row violates it. This register is append-then-transition and never hard
-- deletes, so its history is the evidence of what authority was held and when.
-- A migration must not rewrite that to make a constraint pass. Run
-- ALTER TABLE ... VALIDATE CONSTRAINT once the backlog is known clean.

-- The status vocabulary the domain actually defines. Enforced in Go today and
-- nowhere else, so any other writer -- a fix-up script, a future service --
-- could leave a status no consumer knows how to read.
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_status_known
    CHECK (status IN ('ACTIVE', 'REVOKED', 'EXPIRED')) NOT VALID;

-- A terminal state must carry its evidence. A REVOKED grant with no
-- revoked_at/revoked_by is a record that someone withdrew an authority with no
-- account of who or when -- which is the only thing that row is for. Same for
-- EXPIRED and expired_at, which the lazy sweep stamps.
--
-- Written as an equivalence in both directions: the timestamp must be present
-- when the status says so, and absent when it does not, so a revoked_at cannot
-- linger on a row that was later rewritten to ACTIVE.
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_revoked_has_evidence
    CHECK ((status = 'REVOKED') = (revoked_at IS NOT NULL AND revoked_by_principal_id IS NOT NULL)) NOT VALID;

ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_expired_has_evidence
    CHECK ((status = 'EXPIRED') = (expired_at IS NOT NULL)) NOT VALID;

-- A delegation delegates authority to someone else. A grant whose delegator and
-- delegate are the same principal is not a delegation chain, it is a no-op that
-- reads as one -- and, before the caller/delegator binding added in this pass,
-- it was the shape a self-elevation attempt would have taken.
ALTER TABLE delegation_grants
    ADD CONSTRAINT delegation_grants_delegate_differs
    CHECK (delegator_principal_id <> delegate_principal_id) NOT VALID;

-- The register is read newest-first and is paged now; created_at alone is not a
-- total order, so the index carries the primary key as a tiebreaker for the
-- same reason the ORDER BY does. Without it two grants created in the same
-- transaction can straddle a page boundary and be returned twice, or not at all.
CREATE INDEX IF NOT EXISTS idx_delegation_grants_tenant_created
    ON delegation_grants (tenant_id, created_at DESC, delegation_id DESC);
