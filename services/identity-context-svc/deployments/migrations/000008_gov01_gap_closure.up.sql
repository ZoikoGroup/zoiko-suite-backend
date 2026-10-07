-- 000008_gov01_gap_closure.up.sql
--
-- Closes the schema half of five GOV-01 §4 compliance gaps found in the
-- 2026-09-23 documentation audit. The application half lives in
-- internal/context, internal/store and cmd/server.
--
--   1. session_contexts.source_channel / workload_id / causation_id
--      §4 lists source channel among the SERVER-RESOLVED context and workload
--      identity among the REQUIRED SOURCE INPUTS, and the evidence row is
--      required to carry "correlation/causation IDs" -- plural, and only
--      correlation was ever stored. All three arrived on the canonical
--      envelope and were parsed, propagated and then dropped on the floor, so
--      a decision could not be explained after the fact by the channel it came
--      in on or the workload that made it.
--
--   2. session_contexts.entitlement_context_ref
--      §4 also names an "entitlement context reference" as server-resolved,
--      with the "commercial entitlement read model" as a dependency. NOTHING
--      IN THE ESTATE EXPOSES ONE TODAY -- commercial-account-svc serves
--      accounts, memberships and price catalogs, not entitlement resolution.
--      The column is added now, nullable, so the evidence row has a home for
--      the reference the moment an upstream exists. It is deliberately NOT
--      backfilled or defaulted: a non-null value must mean "an authority
--      resolved this", never "we had nowhere to put a blank".
--
--   3. idempotency_keys
--      §4's Engineering Interaction Wireframe requires "Idempotency-Key +
--      expected_version where stateful" on every COMMAND. The envelope
--      middleware has always REQUIRED the header and nothing has ever READ
--      it: there was no dedupe store, and ErrCodeIdempotencyMismatch was
--      declared in domain/gov01.go and returned by no code path in the
--      service. Replaying POST /v1/context/support with an identical key
--      minted a SECOND break-glass grant -- a privileged, time-limited,
--      independently-approved elevation, duplicated by a retry.
--
--   4. support_contexts reconciler read hatch
--      §1 requires break-glass to be "followed by reconciliation/review".
--      SupportService.Reconcile exists and its own comment claims it is "run
--      on a schedule by the reconciler goroutine in cmd/server" -- there was
--      no such goroutine, and the store method behind it is tenant-scoped, so
--      even once wired it could only ever have swept the tenant that asked.
--      A background sweep has no tenant, so it needs a named hatch.

BEGIN;

-- ---------------------------------------------------------------------------
-- 1 + 2. Decision evidence the audit found missing.
-- ---------------------------------------------------------------------------
--
-- All four are nullable with no default. A NULL here is honest: it says this
-- row predates the column, or the caller presented nothing. Defaulting them to
-- '' would make "absent" and "empty" indistinguishable in exactly the evidence
-- a governance decision is reconstructed from.

ALTER TABLE session_contexts
    ADD COLUMN IF NOT EXISTS source_channel          VARCHAR(64),
    ADD COLUMN IF NOT EXISTS workload_id             VARCHAR(255),
    ADD COLUMN IF NOT EXISTS causation_id            VARCHAR(255),
    ADD COLUMN IF NOT EXISTS entitlement_context_ref VARCHAR(255),
    ADD COLUMN IF NOT EXISTS ingress_binding_version VARCHAR(64);

COMMENT ON COLUMN session_contexts.source_channel IS
    'ZS-SVC-A-001 §4 server-resolved context: the channel the request arrived on (api, web, mobile, batch, system). Read from the canonical envelope, recorded here so a decision can be explained by its channel.';
COMMENT ON COLUMN session_contexts.workload_id IS
    'ZS-SVC-A-001 §4 required source input: the calling workload identity, where one was presented. NULL for ordinary user traffic.';
COMMENT ON COLUMN session_contexts.causation_id IS
    'ZS-SVC-A-001 §4 evidence/lineage requires correlation AND causation. Correlation says "these belong to one story"; causation says "this happened BECAUSE of that".';
COMMENT ON COLUMN session_contexts.ingress_binding_version IS
    'ZS-SVC-A-001 §4 evidence/lineage requires "policy/version references". This is the version of the tenant-registry fact the ingress binding cached, so a decision says not only that the ingress matched but WHICH revision of the routing truth it matched against.';
COMMENT ON COLUMN session_contexts.entitlement_context_ref IS
    'ZS-SVC-A-001 §4 server-resolved entitlement context reference. Always NULL until an entitlement read model exists in the estate to resolve it -- see migration header.';

-- ---------------------------------------------------------------------------
-- 3. Idempotency replay protection.
-- ---------------------------------------------------------------------------
--
-- Keyed by (tenant, endpoint, key) rather than (tenant, key). A key is chosen
-- by the caller and callers reuse them across endpoints; scoping to the
-- endpoint means a client that sends "retry-7" to two different commands is
-- not told its second command is a replay of its first.
--
-- request_fingerprint is what separates a genuine retry from a key collision.
-- Same key + same fingerprint is a retry and MUST return the first response.
-- Same key + different fingerprint is the caller reusing a key for different
-- work, which is the IDEMPOTENCY_MISMATCH case §4 already named and which this
-- service could not previously detect.

CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id           VARCHAR(255) NOT NULL,
    endpoint            VARCHAR(255) NOT NULL,
    idempotency_key     VARCHAR(255) NOT NULL,

    -- SHA-256 of the request body, hex. Not the body itself: a support-context
    -- request carries a justification and a ticket reference, and this table
    -- should not become a second copy of them.
    --
    -- VARCHAR, deliberately NOT CHAR(64). char(n) is blank-padded: Postgres
    -- pads any shorter value out to 64 characters and hands the padding back on
    -- read, so the stored fingerprint no longer equals the one computed from
    -- the request and EVERY retry is reported as IDEMPOTENCY_MISMATCH. A
    -- SHA-256 hex digest happens to be exactly 64 characters, so char(64) would
    -- have worked by luck alone — until somebody changed the hash. The
    -- integration test caught this; the middleware's in-memory fake could not.
    request_fingerprint VARCHAR(64)  NOT NULL,

    -- The first response, replayed verbatim on retry. Status is stored
    -- alongside the body because 201-vs-200 is the difference between "you
    -- created this" and "this already existed", and a replay must not silently
    -- promote one to the other.
    response_status     INT          NOT NULL,
    response_body       JSONB        NOT NULL,

    created_at          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),

    CONSTRAINT idempotency_keys_pk PRIMARY KEY (tenant_id, endpoint, idempotency_key),

    -- 0 is the IN-FLIGHT sentinel: a claim is inserted before the command runs
    -- so a concurrent duplicate cannot also execute, and the real status is
    -- written when the handler returns. Excluding 0 here would make every
    -- claim fail the constraint and every command 503.
    CONSTRAINT idempotency_keys_status_sane
        CHECK (response_status = 0 OR response_status BETWEEN 100 AND 599)
);

-- Supports the retention sweep, which prunes by age. Without it the sweep is a
-- sequential scan over every key the platform has ever seen.
CREATE INDEX IF NOT EXISTS idx_idempotency_keys_created_at
    ON idempotency_keys (created_at);

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE ROW LEVEL SECURITY;

-- FORCE matters here: it applies the policy to the table OWNER too. Without it
-- the service's own role bypasses isolation on the table that decides whether
-- a privileged command has already run.
--
-- Note this forces it for the owner, NOT for a superuser -- a superuser
-- bypasses RLS unconditionally, which is why the store tests must not run as
-- one if they are to prove anything.
CREATE POLICY tenant_isolation_policy ON idempotency_keys
    FOR ALL
    USING (tenant_id = current_setting('app.tenant_id', true))
    WITH CHECK (tenant_id = current_setting('app.tenant_id', true));

-- The retention sweep prunes recorded responses across every tenant, so it
-- needs its own hatch or FORCE RLS silently makes the purge a no-op: it would
-- run, report success, delete nothing, and the table would grow forever.
--
-- DELETE only. The sweep removes expired records; it has no business reading
-- one tenant's command responses or writing any.
CREATE POLICY retention_sweep_purge ON idempotency_keys
    FOR DELETE
    USING (current_setting('app.retention_sweep', true) = 'true');

-- ---------------------------------------------------------------------------
-- 4. Break-glass reconciliation read hatch.
-- ---------------------------------------------------------------------------
--
-- A separate, SELECT-ONLY policy rather than another disjunct bolted onto
-- tenant_isolation_policy. Permissive policies are OR'd, so this widens reads
-- for the sweeper and nothing else.
--
-- The name is deliberately its own: app.support_reconciler, not the outbox
-- relay's app.outbox_relay. Sharing one GUC name between two hatches silently
-- grants each the union of both their privileges -- the relay needs UPDATE,
-- and the reconciler must never have it. The reconciler REPORTS; a human
-- reviews, tenant-scoped, through the API.
CREATE POLICY support_reconciler_read ON support_contexts
    FOR SELECT
    USING (current_setting('app.support_reconciler', true) = 'true');

COMMIT;
