-- 000024_quest_completion_status_by.up.sql
-- Records who changed the status of a quest completion (approved, rejected, or
-- cancelled). Nullable for existing rows and for pending completions.
-- On user deletion, status_by is set to NULL while the completion is preserved.

ALTER TABLE quest_completions
    ADD COLUMN status_by UUID REFERENCES users(id) ON DELETE SET NULL;
