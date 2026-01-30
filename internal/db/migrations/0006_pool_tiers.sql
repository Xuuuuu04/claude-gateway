SET @exists := (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'pools' AND COLUMN_NAME = 'tiers_json');
SET @sql := IF(@exists = 0, 'ALTER TABLE pools ADD COLUMN tiers_json JSON NULL AFTER strategy', 'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Optionally initialize tiers_json from old credential_ids_json for backward compatibility (best effort)
-- For now, we'll leave it NULL and let the UI/API handle the migration when saved.
