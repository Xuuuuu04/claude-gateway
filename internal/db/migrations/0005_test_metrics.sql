SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'credentials' AND COLUMN_NAME = 'last_test_ttft_ms');
SET @sql := IF(@exists = 0, 'ALTER TABLE credentials ADD COLUMN last_test_ttft_ms BIGINT NULL AFTER last_test_latency_ms', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'credentials' AND COLUMN_NAME = 'last_test_tps');
SET @sql := IF(@exists = 0, 'ALTER TABLE credentials ADD COLUMN last_test_tps DOUBLE NULL AFTER last_test_ttft_ms', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
