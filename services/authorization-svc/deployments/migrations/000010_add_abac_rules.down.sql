-- Reverts 000010. Dropping the table drops every declared attribute
-- condition with it, so every action an ABAC rule was denying becomes
-- permitted again the moment this runs. That is a widening of access, which
-- is the direction this service otherwise never fails in — export abac_rules
-- before running this if the rules were load-bearing.
DROP TABLE IF EXISTS abac_rules;
