-- 000010_quests_from_chores.up.sql
-- Copy every chores row into household_quests with the same id and household,
-- name=title, notes=description, points=0, repeat_days=1, nothing claimed,
-- and completed state preserved (available_at = completed_at + 1 day).
-- Chores table is preserved as read-only archive.
-- Idempotent via ON CONFLICT (id) DO NOTHING.

INSERT INTO household_quests (
    id,
    household_id,
    library_id,
    name,
    points,
    repeat_days,
    notes,
    claimed_by,
    claimed_at,
    done_at,
    completed_at,
    available_at,
    created_at,
    updated_at
)
SELECT
    id,
    household_id,
    NULL,
    title,
    0,
    1,
    description,
    NULL,
    NULL,
    NULL,
    completed_at,
    CASE
        WHEN completed_at IS NOT NULL THEN completed_at + INTERVAL '1 day'
        ELSE NULL
    END,
    created_at,
    updated_at
FROM chores
ON CONFLICT (id) DO NOTHING;
