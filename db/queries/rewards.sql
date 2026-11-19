-- Household reward and reward redemption queries are scoped by household_id.
-- Membership is checked by the service layer first; the household_id
-- predicate guarantees a reward or redemption ID from another household can
-- never be read or modified through this one. The reward library is global
-- and read-only: there are no write queries on it. No query here writes
-- family_members: a redemption's points move only on approval, through
-- creditPoints in the service layer.

-- name: ListRewardLibrary :many
SELECT * FROM reward_library ORDER BY cost ASC, description ASC, id ASC;

-- name: GetRewardLibraryItem :one
SELECT * FROM reward_library WHERE id = $1;

-- name: ListHouseholdRewards :many
SELECT * FROM household_rewards
WHERE household_id = $1
ORDER BY created_at ASC, id ASC;

-- name: GetHouseholdReward :one
SELECT * FROM household_rewards WHERE household_id = $1 AND id = $2;

-- name: CreateHouseholdReward :one
INSERT INTO household_rewards (id, household_id, library_id, description, cost, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, NOW(), NOW())
RETURNING *;

-- name: UpdateHouseholdReward :one
UPDATE household_rewards
SET description = $3, cost = $4, updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: DeleteHouseholdReward :execrows
DELETE FROM household_rewards WHERE household_id = $1 AND id = $2;

-- name: CreateRewardRedemption :one
-- A member's request to redeem a reward, waiting for the administrator, with
-- the reward's description and cost snapshotted. A request for a reward the
-- member already has pending writes nothing and returns no row (the partial
-- unique index); the caller then reads the existing request.
INSERT INTO reward_redemptions (id, household_id, member_id, reward_id, description, cost, status, requested_at)
VALUES ($1, $2, $3, $4, $5, $6, 'pending', NOW())
ON CONFLICT (household_id, member_id, reward_id) WHERE status = 'pending' DO NOTHING
RETURNING *;

-- name: GetMemberPendingRedemptionForReward :one
SELECT * FROM reward_redemptions
WHERE household_id = $1 AND member_id = $2 AND reward_id = $3 AND status = 'pending';

-- name: ListPendingRewardRedemptions :many
-- The household's requests waiting for approval, oldest first, each with the
-- requesting member's name.
SELECT r.id, r.household_id, r.member_id, r.reward_id, r.description, r.cost,
       r.status, r.requested_at, r.decided_at, m.name AS member_name
FROM reward_redemptions r
JOIN family_members m ON m.household_id = r.household_id AND m.id = r.member_id
WHERE r.household_id = $1 AND r.status = 'pending'
ORDER BY r.requested_at ASC, r.id ASC;

-- name: ListMemberPendingRedemptions :many
SELECT * FROM reward_redemptions
WHERE household_id = $1 AND member_id = $2 AND status = 'pending'
ORDER BY requested_at ASC, id ASC;

-- name: ListMemberRewardHistory :many
-- The member's approved requests, newest approval first. Each row carries
-- the description and cost snapshotted on the request, so editing or
-- deleting the reward later never changes the history.
SELECT * FROM reward_redemptions
WHERE household_id = $1 AND member_id = $2 AND status = 'approved'
ORDER BY decided_at DESC, id DESC
LIMIT $3 OFFSET $4;

-- name: GetPendingRedemptionMember :one
-- The member of a pending request, read without a lock so Approve and Reject
-- can lock that member's row before they lock the request (member first, the
-- order FamilyService.Remove uses).
SELECT member_id FROM reward_redemptions
WHERE household_id = $1 AND id = $2 AND status = 'pending';

-- name: LockPendingRedemption :one
-- Locks a pending request of the member, after the caller locked the member
-- row. A request decided or cancelled meanwhile returns no row.
SELECT * FROM reward_redemptions
WHERE household_id = $1 AND id = $2 AND member_id = $3 AND status = 'pending'
FOR UPDATE;

-- name: ApproveRewardRedemption :one
UPDATE reward_redemptions
SET status = 'approved', decided_at = NOW(), status_by = $3
WHERE household_id = $1 AND id = $2 AND status = 'pending'
RETURNING *;

-- name: RejectRewardRedemption :one
UPDATE reward_redemptions
SET status = 'rejected', decided_at = NOW(), status_by = $3
WHERE household_id = $1 AND id = $2 AND status = 'pending'
RETURNING *;

-- name: CancelMemberPendingRedemptions :execrows
-- Cancels every request of a member who is leaving the household. The UPDATE
-- locks each row it cancels, after the caller's lock on the member row
-- (member first, the order Approve and Reject use). No points move.
UPDATE reward_redemptions
SET status = 'cancelled', decided_at = NOW()
WHERE household_id = $1 AND member_id = $2 AND status = 'pending';
