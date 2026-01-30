SET @db := DATABASE();

SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = @db AND TABLE_NAME = 'request_logs' AND COLUMN_NAME = 'src_ip');
SET @sql := IF(@exists = 0, 'ALTER TABLE request_logs ADD COLUMN src_ip VARCHAR(64) NULL AFTER client_key', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = @db AND TABLE_NAME = 'request_logs' AND COLUMN_NAME = 'user_agent');
SET @sql := IF(@exists = 0, 'ALTER TABLE request_logs ADD COLUMN user_agent VARCHAR(255) NULL AFTER src_ip', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = @db AND TABLE_NAME = 'request_logs' AND COLUMN_NAME = 'is_test');
SET @sql := IF(@exists = 0, 'ALTER TABLE request_logs ADD COLUMN is_test BOOLEAN NOT NULL DEFAULT FALSE AFTER user_agent', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = @db AND TABLE_NAME = 'request_logs' AND COLUMN_NAME = 'stream');
SET @sql := IF(@exists = 0, 'ALTER TABLE request_logs ADD COLUMN stream BOOLEAN NOT NULL DEFAULT FALSE AFTER is_test', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = @db AND TABLE_NAME = 'request_logs' AND COLUMN_NAME = 'request_bytes');
SET @sql := IF(@exists = 0, 'ALTER TABLE request_logs ADD COLUMN request_bytes INT NULL AFTER stream', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = @db AND TABLE_NAME = 'request_logs' AND COLUMN_NAME = 'response_bytes');
SET @sql := IF(@exists = 0, 'ALTER TABLE request_logs ADD COLUMN response_bytes INT NULL AFTER request_bytes', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
