-- ZS-SVC-Z-001 RB-05 / DQC-017 (docs/architecture/DQC_GAP_MATRIX.md, "scheduled certification sweep").
--
-- The scheduled sweep has to know WHICH (tenant, legal entity) pairs have certifications on the watch list, and certification_watch is
-- tenant-isolated with row-level security FORCEd, so a background job cannot list across tenants. The outbox relay solves the same
-- problem with a transaction-scoped capability (set_config('app.outbox_relay', 'true', true)) that a policy recognises; this is the
-- same pattern, narrower:
--
--   * SELECT only: the policy grants no insert, update or delete, so the capability can read, never change;
--   * transaction-scoped: the setting lasts for the one transaction that sets it;
--   * the scan selects only the two ids it needs (tenant, entity); every sweep then runs under the ordinary tenant isolation.
--
-- Policies of one command are OR-ed, so this adds to tenant isolation and never weakens it for any other statement.

CREATE POLICY certification_sweeper_scan ON certification_watch
    FOR SELECT
    USING (current_setting('app.certification_sweeper', true) = 'true');
