-- 000012_ops_escape_on_values
--
-- The change lifecycle, break-glass activation and the expiry sweep run under
-- the named app.ops_sweep escape (000006), because they act across scopes. But
-- the escape was only admitted on 000006's own tables. The tables those paths
-- actually write — config_entries, feature_flags, event_outbox — kept policies
-- that admit the request's own tenant alone. Under the real runtime role
-- (NOSUPERUSER NOBYPASSRLS) every tenant-scoped change activation, every
-- tenant break-glass activation and revert, and every event those paths
-- enqueue failed WITH CHECK and answered 503. The store suite never saw it: it
-- connects as a superuser, which ignores row-level security.
--
-- Each policy is reproduced exactly with one added disjunct, so request-path
-- isolation is unchanged.

DROP POLICY IF EXISTS tenant_isolation_policy ON config_entries;
CREATE POLICY tenant_isolation_policy ON config_entries
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS tenant_isolation_policy ON feature_flags;
CREATE POLICY tenant_isolation_policy ON feature_flags
    FOR ALL
    USING (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id IS NULL
        OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        OR COALESCE(NULLIF(current_setting('app.snapshot_mint', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );

DROP POLICY IF EXISTS outbox_tenant_isolation ON event_outbox;
CREATE POLICY outbox_tenant_isolation ON event_outbox FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')
        OR COALESCE(NULLIF(current_setting('app.outbox_relay', true), ''), 'false') = 'true'
        OR COALESCE(NULLIF(current_setting('app.ops_sweep', true), ''), 'false') = 'true'
    );
