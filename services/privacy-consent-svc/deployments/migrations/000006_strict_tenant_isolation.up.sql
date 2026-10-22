-- 000006_strict_tenant_isolation.up.sql
--
-- Migrations 000003 and 000005 wrote every tenant policy as
--
--     tenant_id IS NULL OR tenant_id::text = <the session tenant>
--
-- so a row with no tenant was visible and writable from EVERY tenant. This
-- service no longer creates such rows (every mutating route requires a
-- verified X-Tenant-Id and stamps it), so the exception protected nothing and
-- exposed any legacy NULL-tenant evidence — consent receipts, withdrawals,
-- preferences — across tenants, including to withdrawal and invalidation.
--
-- This migration removes the exception. It adds NO platform-scope
-- replacement: ZS-SVC-W-001 §24 says platform records live "in a separate
-- governed plane", and this service has no such plane.
--
-- After this migration a tenant-owned row is visible to, and writable by, its
-- own tenant and nobody else — including sessions with no tenant at all.
--
-- LEGACY ROWS: a row whose tenant_id is NULL becomes invisible to the
-- application role (it is not deleted; consent evidence is append-only). The
-- block below RAISEs a WARNING with the count per table so an operator
-- notices; assigning those rows an owner is a data decision this migration
-- does not make.

DO $$
DECLARE
    t text;
    n bigint;
BEGIN
    FOREACH t IN ARRAY ARRAY['notices', 'presentation_receipts', 'consent_receipts',
                             'withdrawal_receipts', 'preference_assertions',
                             'consent_requests', 'consent_invalidations'] LOOP
        EXECUTE format('SELECT count(*) FROM %I WHERE tenant_id IS NULL', t) INTO n;
        IF n > 0 THEN
            RAISE WARNING 'strict tenant isolation: % row(s) in % have no tenant_id and are now invisible to the application role; they need an operator-assigned owner', n, t;
        END IF;
    END LOOP;
END $$;

-- New rows cannot be created without a tenant. NOT VALID: enforced for every
-- new or changed row, without failing the migration on legacy rows (which the
-- warning above reports instead).
ALTER TABLE notices               ADD CONSTRAINT notices_tenant_assigned               CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE presentation_receipts ADD CONSTRAINT presentation_receipts_tenant_assigned CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE consent_receipts      ADD CONSTRAINT consent_receipts_tenant_assigned      CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE withdrawal_receipts   ADD CONSTRAINT withdrawal_receipts_tenant_assigned   CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE preference_assertions ADD CONSTRAINT preference_assertions_tenant_assigned CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE consent_requests      ADD CONSTRAINT consent_requests_tenant_assigned      CHECK (tenant_id IS NOT NULL) NOT VALID;
ALTER TABLE consent_invalidations ADD CONSTRAINT consent_invalidations_tenant_assigned CHECK (tenant_id IS NOT NULL) NOT VALID;

-- Strict policies. RLS is already ENABLEd and FORCEd on every one of these
-- tables (000003/000005); FORCE is what makes the table owner subject to it.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['notices', 'presentation_receipts', 'consent_receipts',
                             'withdrawal_receipts', 'preference_assertions',
                             'consent_requests', 'consent_invalidations'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation_policy ON %I', t);
        EXECUTE format($p$
            CREATE POLICY tenant_isolation_policy ON %I
                FOR ALL
                USING (tenant_id IS NOT NULL
                       AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
                WITH CHECK (tenant_id IS NOT NULL
                            AND tenant_id::text = NULLIF(current_setting('app.tenant_id', true), ''))
        $p$, t);
    END LOOP;
END $$;

-- notice_versions has no tenant column: it is owned through its notice.
DROP POLICY IF EXISTS tenant_isolation_policy ON notice_versions;
CREATE POLICY tenant_isolation_policy ON notice_versions
    FOR ALL
    USING (
        EXISTS (
            SELECT 1 FROM notices n
            WHERE n.notice_id = notice_versions.notice_id
              AND n.tenant_id IS NOT NULL
              AND n.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM notices n
            WHERE n.notice_id = notice_versions.notice_id
              AND n.tenant_id IS NOT NULL
              AND n.tenant_id::text = NULLIF(current_setting('app.tenant_id', true), '')
        )
    );

-- consent_idempotency_keys is already strict (000004: tenant_id TEXT NOT NULL
-- and `tenant_id = NULLIF(current_setting('app.tenant_id', true), '')`).

-- event_outbox: the tenant clause also matched an empty tenant_id against an
-- empty session tenant. A tenant-owned event now needs a real, matching
-- tenant; only the relay capability crosses tenants.
DROP POLICY IF EXISTS tenant_isolation_policy ON event_outbox;
CREATE POLICY tenant_isolation_policy ON event_outbox
    FOR ALL
    USING (
        current_setting('app.outbox_relay', true) = 'true'
        OR (tenant_id <> '' AND tenant_id = COALESCE(current_setting('app.tenant_id', true), ''))
    )
    WITH CHECK (
        current_setting('app.outbox_relay', true) = 'true'
        OR (tenant_id <> '' AND tenant_id = COALESCE(current_setting('app.tenant_id', true), ''))
    );
