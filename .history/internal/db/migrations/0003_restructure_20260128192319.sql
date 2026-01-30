-- Set default client_key for existing pools if empty (using pool name)
UPDATE pools SET client_key = CONCAT('sk-', LOWER(REPLACE(name, ' ', '-'))) WHERE client_key IS NULL OR client_key = '';

-- Update request_logs for persistence and structure
-- We use a procedure to add columns only if they don't exist
DROP PROCEDURE IF EXISTS AddColumnIfNotExists;
DELIMITER //
CREATE PROCEDURE AddColumnIfNotExists(
    IN tableName VARCHAR(255),
    IN columnName VARCHAR(255),
    IN columnDef VARCHAR(255)
)
BEGIN
    IF NOT EXISTS (
        SELECT * FROM information_schema.COLUMNS
        WHERE TABLE_SCHEMA = DATABASE()
        AND TABLE_NAME = tableName
        AND COLUMN_NAME = columnName
    ) THEN
        SET @sql = CONCAT('ALTER TABLE ', tableName, ' ADD COLUMN ', columnName, ' ', columnDef);
        PREPARE stmt FROM @sql;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END //
DELIMITER ;

CALL AddColumnIfNotExists('pools', 'client_key', 'VARCHAR(255) UNIQUE AFTER name');
CALL AddColumnIfNotExists('pools', 'credential_ids_json', 'JSON AFTER strategy');
CALL AddColumnIfNotExists('pools', 'model_map_json', 'JSON AFTER credential_ids_json');

CALL AddColumnIfNotExists('request_logs', 'provider_id', 'BIGINT UNSIGNED AFTER pool_id');
CALL AddColumnIfNotExists('request_logs', 'credential_id', 'BIGINT UNSIGNED AFTER provider_id');
CALL AddColumnIfNotExists('request_logs', 'client_key', 'VARCHAR(255) AFTER credential_id');
CALL AddColumnIfNotExists('request_logs', 'upstream_model', 'VARCHAR(255) AFTER req_model');
CALL AddColumnIfNotExists('request_logs', 'error_msg', 'TEXT AFTER error_message_hash');

DROP PROCEDURE IF EXISTS AddColumnIfNotExists;

-- Drop redundant tables
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS routing_rules;
DROP TABLE IF EXISTS channel_health;
