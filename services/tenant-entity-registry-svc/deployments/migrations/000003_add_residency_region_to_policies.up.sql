-- Migration: 000003_add_residency_region_to_policies.up.sql
--
-- Closes a real data-model gap found while implementing the Global Traffic
-- & Residency Manager (docs/architecture/global-traffic-residency-manager-
-- design.md, Q2): data_residency_policies stored HOW STRICTLY residency is
-- enforced (residency_mode) but never WHICH region a tenant's data should
-- stay in. Nothing linked a policy to a row in residency_regions.
--
-- Nullable, not backfilled: existing policies predate this column and have
-- no way to infer a correct region automatically — silently guessing one
-- would be worse than leaving it explicitly unresolved. New policies going
-- forward should set this; GTRM's tenant-region lookup endpoint returns
-- "unresolved" for any tenant whose active policy has it NULL.
--
-- Migrations are run via golang-migrate CLI in CI/CD. Do NOT auto-run on
-- service startup.

ALTER TABLE data_residency_policies
    ADD COLUMN residency_region_id UUID REFERENCES residency_regions(residency_region_id);
