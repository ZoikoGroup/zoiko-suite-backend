ALTER TABLE tenant_lifecycle_history
    DROP COLUMN IF EXISTS onboarding_request_ref,
    DROP COLUMN IF EXISTS external_customer_key;