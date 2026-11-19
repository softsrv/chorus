ALTER TABLE reward_redemptions ADD COLUMN status_by UUID REFERENCES users(id) ON DELETE SET NULL;
