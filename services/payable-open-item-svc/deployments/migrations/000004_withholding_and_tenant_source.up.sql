-- ZS-SVC-D-001 AP-08 hardening.
--
-- 1. WITHHOLDING applications. A supplier is paid net of withholding tax;
--    the withheld portion also extinguishes the liability (it is owed to
--    the tax authority instead). It is recorded as its own application,
--    idempotent on the same Banking payment reference as the PAYMENT it
--    accompanies.
-- 2. Source uniqueness per tenant. One payable per (source_type,
--    source_reference) was global, so two tenants with the same invoice
--    number collided. It is now scoped to the tenant.

ALTER TABLE settlement_applications DROP CONSTRAINT IF EXISTS settlement_applications_application_type_check;
ALTER TABLE settlement_applications ADD CONSTRAINT settlement_applications_application_type_check
    CHECK (application_type IN ('PAYMENT', 'WITHHOLDING', 'SUPPLIER_CREDIT', 'RECOVERY'));

CREATE UNIQUE INDEX uq_settlement_applications_withholding_ref
    ON settlement_applications (payable_id, idempotency_ref)
    WHERE application_type = 'WITHHOLDING' AND idempotency_ref <> '';

DROP INDEX IF EXISTS uq_payable_open_items_source;
CREATE UNIQUE INDEX uq_payable_open_items_tenant_source
    ON payable_open_items (COALESCE(tenant_id::text, ''), source_type, source_reference);
