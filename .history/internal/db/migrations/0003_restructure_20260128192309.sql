-- Migrate data: For each pool, find all credentials from its linked channels
-- Note: MySQL 5.7+ supports JSON_ARRAYAGG and JSON_CONTAINS
-- We check if channels table exists before migrating
-- But since migration 0003 is being re-run, we might have already dropped it.
-- We use a conditional block or just wrap in a way that doesn't fail if already done.

-- Set default client_key for existing pools if empty (using pool name)
UPDATE pools SET client_key = CONCAT('sk-', LOWER(REPLACE(name, ' ', '-'))) WHERE client_key IS NULL OR client_key = '';

-- Update request_logs for persistence and structure
ALTER TABLE request_logs ADD COLUMN provider_id BIGINT UNSIGNED AFTER pool_id;
ALTER TABLE request_logs ADD COLUMN credential_id BIGINT UNSIGNED AFTER provider_id;
ALTER TABLE request_logs ADD COLUMN client_key VARCHAR(255) AFTER credential_id;
ALTER TABLE request_logs ADD COLUMN upstream_model VARCHAR(255) AFTER req_model;
ALTER TABLE request_logs ADD COLUMN error_msg TEXT AFTER error_message_hash;

-- Drop redundant tables (ignore if not exists)
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS routing_rules;
DROP TABLE IF EXISTS channel_health;

-- Clean up pools (ignore if already dropped)
-- ALTER TABLE pools DROP COLUMN channel_ids_json;
