-- Rollback for forecasting-svc initial schema.

DROP TABLE IF EXISTS forecast_projections CASCADE;
DROP TABLE IF EXISTS forecast_models CASCADE;
