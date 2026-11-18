-- 000018_admin_points_adjustments.down.sql
-- Back to the 000016 constraints. The old constraints cannot hold an admin
-- row (kind 'admin', possibly negative), so while any exists this migration
-- refuses and changes nothing: going down would otherwise fail midway or
-- silently drop the admin's adjustments. Delete or archive them first.
DO $$
DECLARE
    admin_rows INT;
BEGIN
    SELECT count(*) INTO admin_rows FROM points_awards WHERE kind = 'admin';
    IF admin_rows > 0 THEN
        RAISE EXCEPTION 'cannot roll back admin points adjustments: % admin points_awards row(s) exist', admin_rows;
    END IF;
END
$$;

ALTER TABLE points_awards
    DROP CONSTRAINT points_awards_kind_check,
    DROP CONSTRAINT points_awards_points_check;

ALTER TABLE points_awards
    ADD CONSTRAINT points_awards_kind_check
        CHECK (kind IN ('quest', 'combo', 'streak')),
    ADD CONSTRAINT points_awards_points_check
        CHECK (points >= 0);
