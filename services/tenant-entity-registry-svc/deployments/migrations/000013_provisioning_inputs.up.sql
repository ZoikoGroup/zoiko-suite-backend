-- 000013_provisioning_inputs.up.sql
--
-- ORG-02 §4.2 "Required source inputs: … primary jurisdiction; … residency
-- preference; onboarding evidence" and "Server-resolved context: available
-- regions; plan entitlement; … restricted-jurisdiction checks".
--
-- The residency preference becomes the default residency policy's region —
-- the tenant's home region from birth — so it needs no column of its own.
-- The primary jurisdiction and the subscription whose entitlement was checked
-- are recorded on the tenant as provisioning lineage.

ALTER TABLE tenants
    ADD COLUMN primary_jurisdiction_id UUID,
    ADD COLUMN subscription_id         VARCHAR(255);
