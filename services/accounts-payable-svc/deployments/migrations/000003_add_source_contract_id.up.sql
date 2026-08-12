-- vendor_invoices had no way to record which contract (if any) it was
-- issued against — the spec's Invoice.source_contract_id (nullable).
-- Nullable because not every vendor invoice is tied to a contract; a caller
-- who knows the contract-lifecycle-svc contract_id may supply it, but
-- nothing here fabricates one when there isn't a real contract behind an
-- invoice.
ALTER TABLE vendor_invoices
    ADD COLUMN source_contract_id UUID;
