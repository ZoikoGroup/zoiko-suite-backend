-- 000023_add_com03_snapshot_replay.up.sql
-- A6 (SPEC_DEVIATIONS.md): CreateSnapshot's replay currently recomputes
-- GetEffectiveEntitlements fresh instead of returning the original command's
-- own result, so a replay under the same idempotency key can legitimately
-- differ from what was actually captured if state changed in between. This
-- column lets CreateSnapshot tag every entitlement_snapshots row it writes
-- with a per-command batch id, so a replay can look the exact original rows
-- back up by that id instead of recomputing — the same "look up by the
-- command's own resource id" shape B5/B6/B7 already use elsewhere in this
-- service. Nullable: every other snapshot writer (RecomputeEntitlements, a
-- boundary-driven or dunning-driven recompute) leaves it NULL — only
-- CreateSnapshot's own rows carry one.
ALTER TABLE entitlement_snapshots
    ADD COLUMN snapshot_batch_id TEXT
        CHECK (snapshot_batch_id IS NULL OR snapshot_batch_id ~ '^cesb_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');

CREATE INDEX idx_entitlement_snapshots_batch ON entitlement_snapshots (snapshot_batch_id) WHERE snapshot_batch_id IS NOT NULL;
