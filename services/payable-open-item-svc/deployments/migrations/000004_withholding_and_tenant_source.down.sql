DROP INDEX IF EXISTS uq_payable_open_items_tenant_source;
CREATE UNIQUE INDEX uq_payable_open_items_source ON payable_open_items (source_type, source_reference);

DROP INDEX IF EXISTS uq_settlement_applications_withholding_ref;
ALTER TABLE settlement_applications DROP CONSTRAINT IF EXISTS settlement_applications_application_type_check;
ALTER TABLE settlement_applications ADD CONSTRAINT settlement_applications_application_type_check
    CHECK (application_type IN ('PAYMENT', 'SUPPLIER_CREDIT', 'RECOVERY'));
