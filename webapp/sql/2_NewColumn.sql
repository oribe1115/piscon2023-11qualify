
ALTER TABLE `isu_condition` ADD COLUMN `condition_level` VARCHAR(15) NOT NULL DEFAULT "";


INSERT INTO `latest_isu_condition` (`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`)
    SELECT * FROM (SELECT `jia_isu_uuid`, MAX(timestamp), `is_sitting`, `condition`, `message` FROM isu_condition GROUP BY jia_isu_uuid)
	ON DUPLICATE KEY UPDATE timestamp=VALUES(timestamp),is_sitting=VALUES(is_sitting),condition=VALUES(condition),message=VALUES(message)