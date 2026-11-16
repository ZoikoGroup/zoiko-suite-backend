-- Reverses 000001. Dropping tokenized_values destroys every encrypted value
-- in the vault and makes every token stored by banking-connector-svc and
-- payee-banking-identity-svc undetokenizable. Never run this against an
-- environment holding real tokens.
DROP TABLE IF EXISTS tokenized_values;
DROP FUNCTION IF EXISTS tokenized_values_immutable();
DROP TABLE IF EXISTS customer_keys;
