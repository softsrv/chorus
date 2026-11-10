-- 000010_quests_from_chores.down.sql
DELETE FROM household_quests WHERE id IN (SELECT id FROM chores);
