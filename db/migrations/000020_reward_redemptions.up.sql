-- 000020_reward_redemptions.up.sql
-- Reward redemptions: a member asks to redeem a household reward and the
-- administrator approves or rejects the request. The request keeps a snapshot
-- of the reward's description and cost, so editing or deleting the reward
-- later (reward_id becomes NULL) never changes what an approval charges.
-- Points move only on approval, as a points_awards row of kind 'reward',
-- which is always negative.
CREATE TABLE reward_redemptions (
    id           UUID        PRIMARY KEY,
    household_id UUID        NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    member_id    UUID        NOT NULL,
    reward_id    UUID        REFERENCES household_rewards(id) ON DELETE SET NULL,
    description  TEXT        NOT NULL CHECK (char_length(description) BETWEEN 1 AND 200),
    cost         INT         NOT NULL CHECK (cost >= 0),
    status       TEXT        NOT NULL DEFAULT 'pending'
                             CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled')),
    requested_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    decided_at   TIMESTAMPTZ,
    CHECK ((status = 'pending') = (decided_at IS NULL)),
    FOREIGN KEY (household_id, member_id)
        REFERENCES family_members (household_id, id)
        ON DELETE CASCADE
);

CREATE INDEX idx_reward_redemptions_household_status
    ON reward_redemptions (household_id, status);
CREATE INDEX idx_reward_redemptions_member_decided
    ON reward_redemptions (household_id, member_id, decided_at DESC);
-- One waiting request per member and reward: a repeat request finds the
-- existing one instead of writing a second.
CREATE UNIQUE INDEX idx_reward_redemptions_one_pending
    ON reward_redemptions (household_id, member_id, reward_id)
    WHERE status = 'pending';

-- points_awards may hold kind 'reward' only as a debit (points < 0). Admin
-- rows stay any non-zero amount; quest, combo and streak rows stay >= 0.
ALTER TABLE points_awards
    DROP CONSTRAINT points_awards_kind_check,
    DROP CONSTRAINT points_awards_points_check;

ALTER TABLE points_awards
    ADD CONSTRAINT points_awards_kind_check
        CHECK (kind IN ('quest', 'combo', 'streak', 'admin', 'reward')),
    ADD CONSTRAINT points_awards_points_check
        CHECK ((kind = 'admin' AND points <> 0)
            OR (kind = 'reward' AND points < 0)
            OR (kind NOT IN ('admin', 'reward') AND points >= 0));
