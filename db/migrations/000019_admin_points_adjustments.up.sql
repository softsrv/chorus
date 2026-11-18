-- 000018_admin_points_adjustments.up.sql
-- Admin points adjustments: points_awards.kind may be 'admin', and only an
-- admin row may carry a negative amount (any non-zero whole number). Quest,
-- combo and streak rows keep points >= 0. family_members.points >= 0 is
-- untouched, so a balance still never goes below 0.
ALTER TABLE points_awards
    DROP CONSTRAINT points_awards_kind_check,
    DROP CONSTRAINT points_awards_points_check;

ALTER TABLE points_awards
    ADD CONSTRAINT points_awards_kind_check
        CHECK (kind IN ('quest', 'combo', 'streak', 'admin')),
    ADD CONSTRAINT points_awards_points_check
        CHECK ((kind = 'admin' AND points <> 0) OR (kind <> 'admin' AND points >= 0));
