-- Add new columns to pools
ALTER TABLE pools ADD COLUMN client_key VARCHAR(255) UNIQUE AFTER name;
ALTER TABLE pools ADD COLUMN credential_ids_json JSON AFTER strategy;
ALTER TABLE pools ADD COLUMN model_map_json JSON AFTER credential_ids_json;

-- Migrate data: For each pool, find all credentials from its linked channels
-- Note: MySQL 5.7+ supports JSON_ARRAYAGG and JSON_CONTAINS
UPDATE pools p
SET p.credential_ids_json = (
    SELECT JSON_ARRAYAGG(c.credential_id)
    FROM channels c
    WHERE JSON_CONTAINS(p.channel_ids_json, CAST(c.id AS JSON))
);

-- Set default client_key for existing pools if empty (using pool name)
UPDATE pools SET client_key = CONCAT('sk-', LOWER(REPLACE(name, ' ', '-'))) WHERE client_key IS NULL;

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

-- Clean up pools
ALTER TABLE pools DROP COLUMN channel_ids_json;
