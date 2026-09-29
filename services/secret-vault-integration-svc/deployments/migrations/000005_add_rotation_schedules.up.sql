-- Migration: 000005_add_rotation_schedules.up.sql
--
-- Automated rotation (context.md §7.2 rotate + the compliance audit's
-- §13 "no automated rotation" finding): a per-version schedule that says
-- "rotate this policy's material every N seconds." Modeled as its own
-- table (not a column on secret_policy_versions) so the heavily shared
-- version row and its scans are untouched, and so the "when is this
-- version next due" state is explicit and queryable.
--
-- The sweep itself is a privileged, platform-scoped operation (it must
-- revoke every tenant's leases on the rotated path, exactly like
-- POST /v1/secret-policies/{id}/rotate), so the rotation sweeper runs
-- through the same platform-scope path as ActivateVersion/Rotate — see
-- 000003's comment on app.platform_scope. secret_policy_versions has no
-- tenant_id of its own here, and schedules carry no tenant data, so no
-- tenant isolation policy applies to this table.

CREATE TABLE secret_rotation_schedules (
    secret_policy_version_id UUID PRIMARY KEY REFERENCES secret_policy_versions(secret_policy_version_id),
    interval_seconds         INTEGER NOT NULL CHECK (interval_seconds > 0),
    next_rotation_at         TIMESTAMPTZ NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The sweep query: due rows first, and a small index so
-- ListDueRotations(now) is a range scan, not a full table read.
CREATE INDEX idx_secret_rotation_schedules_due
    ON secret_rotation_schedules (next_rotation_at);