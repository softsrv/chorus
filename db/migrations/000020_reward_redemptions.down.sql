-- 000020_reward_redemptions.down.sql
-- Back to the 000019 constraints and no reward_redemptions table. The 000019
-- constraints cannot hold a 'reward' points award, so while any exists this
-- migration refuses and changes nothing: going down would otherwise fail
-- midway or silently drop redeemed points from members' histories. Delete or
-- archive them first.
DO $$
DECLARE
    reward_rows INT;
BEGIN
    SELECT count(*) INTO reward_rows FROM points_awards WHERE kind = 'reward';
    IF reward_rows > 0 THEN
        RAISE EXCEPTION 'cannot roll back reward redemptions: % reward points_awards row(s) exist', reward_rows;
    END IF;
END
$$;

ALTER TABLE points_awards
    DROP CONSTRAINT points_awards_kind_check,
    DROP CONSTRAINT points_awards_points_check;

ALTER TABLE points_awards
    ADD CONSTRAINT points_awards_kind_check
        CHECK (kind IN ('quest', 'combo', 'streak', 'admin')),
    ADD CONSTRAINT points_awards_points_check
        CHECK ((kind = 'admin' AND points <> 0) OR (kind <> 'admin' AND points >= 0));

DROP TABLE reward_redemptions;
