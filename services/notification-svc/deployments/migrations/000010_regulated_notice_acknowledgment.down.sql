-- Down migration for 000010_regulated_notice_acknowledgment.up.sql

DROP POLICY IF EXISTS acknowledgment_chain_tenant_isolation ON acknowledgment_chain;
DROP TABLE IF EXISTS acknowledgment_chain;

DROP POLICY IF EXISTS regulated_notices_tenant_isolation ON regulated_notices;
DROP TABLE IF EXISTS regulated_notices;