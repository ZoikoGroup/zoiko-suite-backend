-- 000010_emergency_reversion
--
-- INV-15: emergency configuration has explicit expiry and a mandatory
-- retrospective. 000006 gave emergency_changes an expiry column and the
-- columns an exact reversion needs (activated_config_entry_id,
-- prior_config_entry_id, reverted_to_prior), but nothing ever wrote them:
-- activation set status ACTIVE and stopped, and the sweep only matured OPEN
-- rows. An activated break-glass value therefore stayed in force forever,
-- with no reversion and no retrospective.
--
-- The existing columns reference config_entries only, and emergency changes
-- also apply to feature flags. So the store records the reversion basis
-- here, for both kinds:
--   kind              'config' | 'flag'
--   activated_row_id  the config_entries.config_id / feature_flags.flag_id
--                     the activation wrote — expiry reverts only if that row
--                     is still the current one (a later governed change wins)
--   had_prior         whether a value existed at that scope before
--   prior_value       that value: the config JSON, or {enabled, rollout_percentage}
--   retrospective_reference  the review record that closes the retrospective

ALTER TABLE emergency_changes
    ADD COLUMN IF NOT EXISTS kind VARCHAR(8)
        CONSTRAINT chk_emergency_changes_kind CHECK (kind IN ('config','flag')),
    ADD COLUMN IF NOT EXISTS activated_row_id UUID,
    ADD COLUMN IF NOT EXISTS activated_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS had_prior BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS prior_value JSONB,
    ADD COLUMN IF NOT EXISTS retrospective_reference TEXT;

-- A closed retrospective must name its review record.
ALTER TABLE emergency_changes DROP CONSTRAINT IF EXISTS chk_emergency_changes_retrospective_closed;
ALTER TABLE emergency_changes ADD CONSTRAINT chk_emergency_changes_retrospective_closed CHECK (
    status <> 'CLOSED'
    OR (retrospective_closed_at IS NOT NULL AND COALESCE(btrim(retrospective_reference), '') <> '')
);
