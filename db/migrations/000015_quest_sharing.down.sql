-- 000015_quest_sharing.down.sql
-- Back to quests that only one member can hold. The old schema has nowhere to
-- record a second member on a completion, so while any pending completion
-- still covers more than one member, this migration refuses and changes
-- nothing: going down would silently drop those members' awards. Approve,
-- send back or otherwise resolve those completions first. Approved shared
-- completions keep their row (member_id is the member who marked it done);
-- the points already credited stay on every member's balance.
DO $$
DECLARE
    shared_pending INT;
BEGIN
    SELECT count(*) INTO shared_pending
    FROM (
        SELECT m.completion_id
        FROM quest_completion_members m
        JOIN quest_completions c ON c.household_id = m.household_id AND c.id = m.completion_id
        WHERE c.status = 'pending'
        GROUP BY m.completion_id
        HAVING count(*) > 1
    ) s;
    IF shared_pending > 0 THEN
        RAISE EXCEPTION 'cannot roll back quest sharing: % pending completion(s) cover more than one member; approve or send them back first', shared_pending;
    END IF;
END
$$;

DROP TABLE quest_completion_members;

ALTER TABLE quest_completions
    DROP CONSTRAINT quest_completions_household_id_id_key;

DROP TABLE household_quest_participants;

ALTER TABLE household_quests
    DROP CONSTRAINT household_quests_teamwork_bonus_shareable_check,
    DROP CONSTRAINT household_quests_teamwork_bonus_check;
ALTER TABLE household_quests
    DROP COLUMN teamwork_bonus,
    DROP COLUMN shareable;
