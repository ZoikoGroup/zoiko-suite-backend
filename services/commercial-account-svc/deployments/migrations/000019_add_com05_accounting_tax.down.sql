-- 000019_add_com05_accounting_tax.down.sql

DROP TABLE IF EXISTS billing_tax_jurisdictions;

ALTER TABLE billing_accounts DROP CONSTRAINT IF EXISTS billing_accounts_accounting_mapping_key_not_blank;
ALTER TABLE billing_accounts DROP COLUMN IF EXISTS accounting_mapping_key;
