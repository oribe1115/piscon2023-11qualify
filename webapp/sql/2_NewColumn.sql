
ALTER TABLE `isu_condition` ADD COLUMN `condition_level` VARCHAR(15) NOT NULL DEFAULT "";

INSERT INTO `latest_isu_condition` (`jia_isu_uuid`, `timestamp`, `is_sitting`, `condition`, `message`)
(
    SELECT t.`jia_isu_uuid`, t.`timestamp`, t.`is_sitting`, t.`condition`, t.`message` FROM `isu_condition` AS t
    JOIN (SELECT `jia_isu_uuid`, MAX(timestamp) FROM isu_condition GROUP BY jia_isu_uuid) AS t2 ON t.`jia_isu_uuid` = t2.`jia_isu_uuid` AND t.`timestamp` = t2.`timestamp`
);
