-- Rollback for anomaly-detection-svc initial schema.

DROP TABLE IF EXISTS anomaly_records CASCADE;
DROP TABLE IF EXISTS anomaly_detection_rules CASCADE;
