-- 000012_combos.down.sql
-- Reverse of 000012_combos.up.sql in dependency order.

DROP TABLE IF EXISTS combo_award_completions;
DROP TABLE IF EXISTS combo_awards;
DROP TABLE IF EXISTS quest_completions;
DROP TABLE IF EXISTS combo_quests;
DROP TABLE IF EXISTS combos;

ALTER TABLE household_quests DROP CONSTRAINT IF EXISTS household_quests_household_id_id_key;
