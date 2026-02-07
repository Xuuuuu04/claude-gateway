-- Update pools: Set default client_key for existing pools if empty (using pool name)
-- Note: columns already added in previous failed run
UPDATE pools SET client_key = CONCAT('sk-', LOWER(REPLACE(name, ' ', '-'))) WHERE client_key IS NULL OR client_key = '';

-- Update request_logs for persistence and structure
ALTER TABLE request_logs ADD COLUMN provider_id BIGINT UNSIGNED AFTER pool_id;
ALTER TABLE request_logs ADD COLUMN credential_id BIGINT UNSIGNED AFTER provider_id;
ALTER TABLE request_logs ADD COLUMN client_key VARCHAR(255) AFTER credential_id;
ALTER TABLE request_logs ADD COLUMN upstream_model VARCHAR(255) AFTER req_model;
ALTER TABLE request_logs ADD COLUMN error_msg TEXT AFTER error_message_hash;

-- Drop redundant tables
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS routing_rules;
DROP TABLE IF EXISTS channel_health;
