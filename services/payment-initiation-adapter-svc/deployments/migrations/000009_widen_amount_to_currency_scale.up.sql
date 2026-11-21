-- The amount column was NUMERIC(18,2): a fixed "2 decimal places" ceiling
-- applied to every currency, not a real rule. A currency's actual allowed
-- decimal places come from its own ISO 4217 minor-unit count (USD/GBP = 2,
-- JPY = 0, most Gulf currencies like BHD = 3) -- resolved per-request from
-- currency-registry-svc (see internal/currency, internal/domain/decimal.go's
-- ValidateCurrencyPrecision). A column scale of 2 would silently round a
-- legitimate 3-decimal BHD amount before the application-level check could
-- even see it, so the column itself has to widen before the per-currency
-- check means anything.
--
-- NUMERIC(18,4) matches treasury-svc's own MoneyScale for the same reason
-- treasury-svc chose it: no real ISO 4217 currency needs more than 4
-- decimal places, and this is a storage ceiling applied uniformly, not a
-- business rule — the per-currency rule is enforced in application code on
-- top of this, exactly as treasury-svc's own ValidateCurrencyPrecision does.
ALTER TABLE payment_initiation_attempts
    ALTER COLUMN amount TYPE NUMERIC(18, 4);
