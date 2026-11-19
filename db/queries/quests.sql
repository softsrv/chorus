-- Household quest queries are scoped by household_id. Membership is checked
-- by the service layer first; the household_id predicate guarantees a quest ID
-- from another household can never be read or modified through this one.
-- The quest library is global and read-only: there are no write queries on it.

-- name: ListQuestLibrary :many
SELECT * FROM quest_library ORDER BY name ASC, id ASC;

-- name: GetQuestLibraryItem :one
SELECT * FROM quest_library WHERE id = $1;

-- name: ListHouseholdQuests :many
-- awaiting_approval: the quest has a completion waiting for the administrator.
-- participants: the members on the quest when it is shared, earliest first.
SELECT sqlc.embed(q), EXISTS (
    SELECT 1 FROM quest_completions c
    WHERE c.household_id = q.household_id AND c.quest_id = q.id AND c.status = 'pending'
) AS awaiting_approval,
ARRAY(
    SELECT p.member_id FROM household_quest_participants p
    WHERE p.household_id = q.household_id AND p.quest_id = q.id
    ORDER BY p.joined_at, p.member_id
)::uuid[] AS participants
FROM household_quests q
WHERE q.household_id = sqlc.arg(household_id)
ORDER BY q.created_at ASC, q.id ASC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: GetHouseholdQuest :one
SELECT * FROM household_quests WHERE household_id = $1 AND id = $2;

-- name: GetHouseholdQuestView :one
-- One quest with awaiting_approval and participants, as ListHouseholdQuests
-- reads it.
SELECT sqlc.embed(q), EXISTS (
    SELECT 1 FROM quest_completions c
    WHERE c.household_id = q.household_id AND c.quest_id = q.id AND c.status = 'pending'
) AS awaiting_approval,
ARRAY(
    SELECT p.member_id FROM household_quest_participants p
    WHERE p.household_id = q.household_id AND p.quest_id = q.id
    ORDER BY p.joined_at, p.member_id
)::uuid[] AS participants
FROM household_quests q
WHERE q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id);

-- name: QuestHasPendingCompletion :one
-- Whether the quest has a completion waiting for the administrator.
SELECT EXISTS (
    SELECT 1 FROM quest_completions c
    WHERE c.household_id = sqlc.arg(household_id) AND c.quest_id = sqlc.arg(quest_id)
      AND c.status = 'pending'
) AS awaiting_approval;

-- name: CreateHouseholdQuest :one
-- min_age and max_age are the quest's inclusive age limits; NULL is no limit
-- on that side.
INSERT INTO household_quests (id, household_id, library_id, name, points, repeat_days, notes, shareable, teamwork_bonus, streak_bonus, min_age, max_age, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, sqlc.narg(min_age), sqlc.narg(max_age), NOW(), NOW())
RETURNING *;

-- name: UpdateHouseholdQuest :one
-- Full replacement of the editable fields. A new points, shareable or
-- teamwork_bonus applies from the next approval: a pending completion is
-- paid from the quest as it stands when it is approved. A new age range
-- applies from the next accept or share: a member who already holds or is on
-- the quest keeps it.
UPDATE household_quests
SET name = $3, points = $4, repeat_days = $5, notes = $6,
    shareable = $7, teamwork_bonus = $8, streak_bonus = $9,
    min_age = sqlc.narg(min_age), max_age = sqlc.narg(max_age), updated_at = NOW()
WHERE household_id = $1 AND id = $2
RETURNING *;

-- name: DeleteHouseholdQuest :execrows
DELETE FROM household_quests WHERE household_id = $1 AND id = $2;

-- name: ClaimQuest :one
-- Assigns an available quest to a family member. The availability check and
-- the write are one statement, so of two concurrent claims exactly one
-- matches; the other gets no row. The claiming member must also be within
-- the quest's age range (quest_age_eligible, in the household's timezone: no
-- birthday or no limits always pass), read only from this household's
-- members; a member out of range, or not in this household, gets no row.
-- Accepting stamps claim_expires_at: the household-local midnight that ends
-- today (quest_claim_expires_at), after which the expiry sweep returns the
-- quest to the pool.
UPDATE household_quests
SET claimed_by = sqlc.arg(member_id), claimed_at = NOW(), updated_at = NOW(),
    claim_expires_at = (
        SELECT quest_claim_expires_at(NOW(), h.timezone)
        FROM households h WHERE h.id = household_quests.household_id
    )
WHERE household_quests.household_id = sqlc.arg(household_id) AND household_quests.id = sqlc.arg(id)
  AND household_quests.claimed_by IS NULL
  AND (household_quests.available_at IS NULL OR household_quests.available_at <= NOW())
  AND EXISTS (
      SELECT 1 FROM family_members m
      JOIN households h ON h.id = m.household_id
      WHERE m.household_id = sqlc.arg(household_id) AND m.id = sqlc.arg(member_id)
        AND quest_age_eligible(m.birthday, h.timezone, household_quests.min_age, household_quests.max_age)
  )
RETURNING *;

-- name: ListIneligibleMemberIDs :many
-- For each of the given quests of the household, the household's members
-- whose birthday puts them outside its age range today (quest_age_eligible,
-- the same rule ClaimQuest and ShareQuest apply). Members without a birthday
-- and quests without limits never appear. Only this household's members are
-- read.
SELECT q.id AS quest_id, m.id AS member_id
FROM household_quests q
JOIN households h ON h.id = q.household_id
JOIN family_members m ON m.household_id = q.household_id
WHERE q.household_id = sqlc.arg(household_id)
  AND q.id = ANY(sqlc.arg(quest_ids)::uuid[])
  AND (q.min_age IS NOT NULL OR q.max_age IS NOT NULL)
  AND NOT quest_age_eligible(m.birthday, h.timezone, q.min_age, q.max_age)
ORDER BY q.id, m.created_at, m.id;

-- name: LockHouseholdQuest :one
-- Locks one quest row of the household for the rest of the transaction,
-- taken in a statement of its own after the caller's member locks. A
-- statement run after it reads the quest's participants in a snapshot that
-- already holds every share, abandon, mark-done or removal of that quest that
-- committed while the lock waited (each takes this row lock), and none can
-- commit under it.
SELECT id FROM household_quests
WHERE household_id = sqlc.arg(household_id) AND id = sqlc.arg(id)
FOR UPDATE;

-- name: AbandonQuest :one
-- The acting member leaves a quest they hold or are on. Only their part goes:
-- their participant row. If they held it, the holder of record moves to the
-- earliest remaining participant (joined_at, then member_id), who keeps
-- their joined_at as claimed_at and keeps the claim deadline
-- (claim_expires_at); a quest with nobody left is back in the pool, done_at
-- and claim_expires_at cleared with the holder so the done_at-needs-a-holder
-- CHECK holds. A quest left with one member is no longer shared, so that member's
-- participant row goes too. A member not on the quest, or a quest not in
-- this household, matches no row and changes nothing. The caller holds the
-- member locks and then the quest's row lock (LockHouseholdQuest, a statement
-- of its own), so the participants counted here are the committed ones: a
-- concurrent share or abandon of the quest is either in this snapshot or
-- waits until this transaction ends.
WITH mine AS (
    SELECT q.household_id, q.id,
           q.claimed_by IS NOT DISTINCT FROM sqlc.arg(member_id)::uuid AS holder,
           nxt.member_id AS next_member, nxt.joined_at AS next_joined_at,
           (SELECT count(*) FROM household_quest_participants r
            WHERE r.household_id = q.household_id AND r.quest_id = q.id
              AND r.member_id <> sqlc.arg(member_id)::uuid) AS remaining
    FROM household_quests q
    LEFT JOIN LATERAL (
        SELECT p.member_id, p.joined_at FROM household_quest_participants p
        WHERE p.household_id = q.household_id AND p.quest_id = q.id
          AND p.member_id <> sqlc.arg(member_id)::uuid
        ORDER BY p.joined_at ASC, p.member_id ASC
        LIMIT 1
    ) nxt ON true
    WHERE q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id)
      AND (q.claimed_by = sqlc.arg(member_id)::uuid OR EXISTS (
          SELECT 1 FROM household_quest_participants p
          WHERE p.household_id = q.household_id AND p.quest_id = q.id
            AND p.member_id = sqlc.arg(member_id)::uuid
      ))
    FOR UPDATE OF q
), left_quest AS (
    DELETE FROM household_quest_participants p
    USING mine m
    WHERE p.household_id = m.household_id AND p.quest_id = m.id
      AND (p.member_id = sqlc.arg(member_id)::uuid OR m.remaining <= 1)
    RETURNING p.quest_id
)
UPDATE household_quests q
SET claimed_by = CASE WHEN m.holder THEN m.next_member ELSE q.claimed_by END,
    claimed_at = CASE WHEN NOT m.holder THEN q.claimed_at
                      WHEN m.next_member IS NULL THEN NULL
                      ELSE m.next_joined_at END,
    done_at = CASE WHEN m.holder AND m.next_member IS NULL THEN NULL ELSE q.done_at END,
    claim_expires_at = CASE WHEN m.holder AND m.next_member IS NULL THEN NULL ELSE q.claim_expires_at END,
    updated_at = NOW()
FROM mine m
WHERE q.household_id = m.household_id AND q.id = m.id
RETURNING q.id, q.household_id, q.library_id, q.name, q.points, q.repeat_days, q.notes,
          q.claimed_by, q.claimed_at, q.done_at, q.completed_at, q.available_at,
          q.created_at, q.updated_at, q.shareable, q.teamwork_bonus, q.streak_bonus,
          q.min_age, q.max_age, q.claim_expires_at;

-- name: ShareQuest :many
-- The holder of a shareable quest adds another member of the household to
-- it. One statement: the quest must be in this household, shareable and held
-- by the acting member, and the target must be a household member other than
-- the actor who is not on the quest yet and is within the quest's age range
-- (quest_age_eligible, in the household's timezone: no birthday or no limits
-- always pass); otherwise nothing is written. A hold whose claim has run out
-- (claim_expires_at at or before now, the end of the day it was accepted) is
-- not held, even before the expiry sweep has released it. The
-- first share also records the holder, joined at the time they accepted, so
-- the holder stays the earliest member. Returns the member ids it added.
WITH quest AS (
    SELECT q.household_id, q.id, q.claimed_at, q.min_age, q.max_age
    FROM household_quests q
    WHERE q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id)
      AND q.shareable AND q.claimed_by = sqlc.arg(member_id)::uuid
      AND q.claim_expires_at > NOW()
    -- Locked and re-checked, so a share racing the holder's mark-done,
    -- abandon or the expiry sweep either sees the quest still held or writes
    -- nothing.
    FOR UPDATE OF q
), target AS (
    SELECT m.id
    FROM family_members m
    JOIN households h ON h.id = m.household_id
    CROSS JOIN quest qa
    WHERE m.household_id = sqlc.arg(household_id) AND m.id = sqlc.arg(target_member_id)::uuid
      AND m.id <> sqlc.arg(member_id)::uuid
      AND quest_age_eligible(m.birthday, h.timezone, qa.min_age, qa.max_age)
      AND NOT EXISTS (
          SELECT 1 FROM household_quest_participants p
          WHERE p.household_id = sqlc.arg(household_id) AND p.quest_id = sqlc.arg(id)
            AND p.member_id = m.id
      )
)
INSERT INTO household_quest_participants (household_id, quest_id, member_id, joined_at)
SELECT q.household_id, q.id, v.member_id, v.joined_at
FROM quest q
CROSS JOIN target t
CROSS JOIN LATERAL (VALUES
    (sqlc.arg(member_id)::uuid, COALESCE(q.claimed_at, NOW())),
    (t.id, NOW())
) AS v(member_id, joined_at)
ON CONFLICT (quest_id, member_id) DO NOTHING
RETURNING member_id;

-- name: QuestClaimLapsed :one
-- Whether a quest of the household is held past its claim deadline
-- (claim_expires_at at or before now). NOW() is the transaction's start, so
-- inside one transaction this agrees with ShareQuest's and MarkQuestDone's
-- own expiry checks. A quest not in this household gets no row.
SELECT (q.claim_expires_at IS NULL OR q.claim_expires_at <= NOW())::boolean AS lapsed
FROM household_quests q
WHERE q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id);

-- name: QuestMemberAgeEligible :one
-- Check whether a member of a household is age-eligible for a quest in that
-- household. Returns quest_age_eligible using the household timezone and quest
-- age limits. Filters family_members, households and household_quests by
-- household_id.
SELECT quest_age_eligible(m.birthday, h.timezone, q.min_age, q.max_age)::boolean AS eligible
FROM family_members m
JOIN households h ON h.id = m.household_id
JOIN household_quests q ON q.household_id = m.household_id
WHERE m.household_id = sqlc.arg(household_id)
  AND h.id = sqlc.arg(household_id)
  AND q.household_id = sqlc.arg(household_id)
  AND m.id = sqlc.arg(member_id)::uuid
  AND q.id = sqlc.arg(quest_id)::uuid;

-- name: ListQuestParticipants :many
-- The members on one quest, earliest first; none when it is not shared.
SELECT member_id FROM household_quest_participants
WHERE household_id = sqlc.arg(household_id) AND quest_id = sqlc.arg(quest_id)
ORDER BY joined_at ASC, member_id ASC;

-- name: LockMemberQuests :many
-- Locks, in id order, every quest of the household that the member holds or
-- is on, before ReleaseMemberQuests reads them. ReleaseMemberQuests counts
-- the members left on each quest; it runs as a statement of its own after
-- this lock, so its snapshot already holds every share, abandon or mark-done
-- that committed while the lock waited (each takes the quest row lock), and
-- none can commit under it.
SELECT q.id
FROM household_quests q
WHERE q.household_id = sqlc.arg(household_id)
  AND (q.claimed_by = sqlc.arg(member_id)::uuid OR EXISTS (
      SELECT 1 FROM household_quest_participants p
      WHERE p.household_id = q.household_id AND p.quest_id = q.id
        AND p.member_id = sqlc.arg(member_id)::uuid
  ))
ORDER BY q.id
FOR UPDATE OF q;

-- name: ReleaseMemberQuests :exec
-- Takes a member who is leaving the household off every quest they are on.
-- The caller has locked those quests first (LockMemberQuests), so the
-- members counted here are the ones committed on each quest.
-- Their participant rows go. On a quest they hold, the holder of record
-- moves to the earliest remaining participant (joined_at, then member_id),
-- who keeps their joined_at as claimed_at and keeps the claim deadline
-- (claim_expires_at); a quest with no participant left is returned to the
-- pool, done_at and claim_expires_at cleared with the holder so the
-- done_at-needs-a-holder CHECK holds. A quest left with one member is no
-- longer shared, so that member's participant row goes too. The quests are
-- locked after the caller's lock on the member row (member first). No
-- points move.
WITH mine AS (
    SELECT q.household_id, q.id,
           q.claimed_by IS NOT DISTINCT FROM sqlc.arg(member_id)::uuid AS holder,
           nxt.member_id AS next_member, nxt.joined_at AS next_joined_at,
           (SELECT count(*) FROM household_quest_participants r
            WHERE r.household_id = q.household_id AND r.quest_id = q.id
              AND r.member_id <> sqlc.arg(member_id)::uuid) AS remaining
    FROM household_quests q
    LEFT JOIN LATERAL (
        SELECT p.member_id, p.joined_at FROM household_quest_participants p
        WHERE p.household_id = q.household_id AND p.quest_id = q.id
          AND p.member_id <> sqlc.arg(member_id)::uuid
        ORDER BY p.joined_at ASC, p.member_id ASC
        LIMIT 1
    ) nxt ON true
    WHERE q.household_id = sqlc.arg(household_id)
      AND (q.claimed_by = sqlc.arg(member_id)::uuid OR EXISTS (
          SELECT 1 FROM household_quest_participants p
          WHERE p.household_id = q.household_id AND p.quest_id = q.id
            AND p.member_id = sqlc.arg(member_id)::uuid
      ))
    FOR UPDATE OF q
), left_quests AS (
    DELETE FROM household_quest_participants p
    USING mine m
    WHERE p.household_id = m.household_id AND p.quest_id = m.id
      AND (p.member_id = sqlc.arg(member_id)::uuid OR m.remaining <= 1)
    RETURNING p.quest_id
)
UPDATE household_quests q
SET claimed_by = m.next_member,
    claimed_at = CASE WHEN m.next_member IS NULL THEN NULL ELSE m.next_joined_at END,
    done_at = CASE WHEN m.next_member IS NULL THEN NULL ELSE q.done_at END,
    claim_expires_at = CASE WHEN m.next_member IS NULL THEN NULL ELSE q.claim_expires_at END,
    updated_at = NOW()
FROM mine m
WHERE q.household_id = m.household_id AND q.id = m.id AND m.holder;

-- name: ListExpiredQuestClaims :many
-- Held quests whose claim has run out (claim_expires_at, the household-local
-- midnight ending the day the quest was accepted, is now or past), across
-- every household, earliest deadline first, at most max_rows per run. The
-- expiry sweep releases each one in a transaction of its own
-- (ExpireQuestClaim); what it misses is listed again on the next run.
SELECT q.household_id, q.id, q.claimed_by
FROM household_quests q
WHERE q.claimed_by IS NOT NULL AND q.claim_expires_at <= NOW()
ORDER BY q.claim_expires_at, q.id
LIMIT sqlc.arg(max_rows);

-- name: ExpireQuestClaim :one
-- Returns a quest whose claim has run out to the pool: every member on it
-- drops it (all its participant rows go) and the holder, claimed_at, done_at
-- and claim_expires_at are cleared, in one statement. No completion is
-- recorded, no points move and available_at is left as it is. The expiry is
-- re-checked here: the caller holds the row locks of every member on the
-- quest and then the quest's row lock (LockHouseholdQuest), so a quest marked
-- done, abandoned or released meanwhile is no longer held and matches no row.
-- A quest not in this household matches no row either.
WITH expired AS (
    SELECT q.household_id, q.id
    FROM household_quests q
    WHERE q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id)
      AND q.claimed_by IS NOT NULL AND q.claim_expires_at <= NOW()
), dropped AS (
    DELETE FROM household_quest_participants p
    USING expired e
    WHERE p.household_id = e.household_id AND p.quest_id = e.id
    RETURNING p.member_id
)
UPDATE household_quests q
SET claimed_by = NULL, claimed_at = NULL, done_at = NULL, claim_expires_at = NULL,
    updated_at = NOW()
FROM expired e
WHERE q.household_id = e.household_id AND q.id = e.id
RETURNING q.id;

-- name: ExpireHouseholdQuestClaims :execrows
-- The household's part of the end-of-day rule (INT-0176), at now: every held
-- quest of the household whose claim has run out (claim_expires_at at or
-- before now) goes back to the pool exactly as ExpireQuestClaim leaves it:
-- every member on it drops it and the holder, claimed_at, done_at and
-- claim_expires_at are cleared. No completion is recorded, no points move and
-- available_at is left as it is. The caller holds the household's member
-- locks first.
WITH expired AS (
    SELECT q.household_id, q.id
    FROM household_quests q
    WHERE q.household_id = sqlc.arg(household_id)
      AND q.claimed_by IS NOT NULL AND q.claim_expires_at <= sqlc.arg(now)::timestamptz
    ORDER BY q.id
    FOR UPDATE OF q
), dropped AS (
    DELETE FROM household_quest_participants p
    USING expired e
    WHERE p.household_id = e.household_id AND p.quest_id = e.id
    RETURNING p.member_id
)
UPDATE household_quests q
SET claimed_by = NULL, claimed_at = NULL, done_at = NULL, claim_expires_at = NULL,
    updated_at = NOW()
FROM expired e
WHERE q.household_id = e.household_id AND q.id = e.id;

-- name: LockMemberPendingCompletions :many
-- Locks, in id order, every pending completion of the household that the
-- member marked done or is recorded on, before CancelMemberPendingCompletions
-- reads them, so its earliest remaining member is read after any concurrent
-- removal of another member on the same completion has committed.
SELECT c.id
FROM quest_completions c
WHERE c.household_id = sqlc.arg(household_id) AND c.status = 'pending'
  AND (c.member_id = sqlc.arg(member_id)::uuid OR EXISTS (
      SELECT 1 FROM quest_completion_members cm
      WHERE cm.household_id = c.household_id AND cm.completion_id = c.id
        AND cm.member_id = sqlc.arg(member_id)::uuid
  ))
ORDER BY c.id
FOR UPDATE OF c;

-- name: CancelMemberPendingCompletions :exec
-- Takes a member who is leaving the household off every completion still
-- waiting for approval. Their quest_completion_members row goes. Where they
-- marked it done, the marker moves first to the earliest remaining recorded
-- member (added_at, then member_id), so the item stays listed and
-- approvable and the marker key is never nulled under it; the rest are paid
-- as the remaining n at approval. A completion with no recorded member left
-- (a lone member's, or one recorded before quest sharing) is cancelled. No
-- points move. As on reject, a quest whose running cooldown a cancelled
-- completion started (the quest's completed_at is its done_at, available_at
-- still ahead) is available again right away: the chore was never approved,
-- so it does not sit resting. Cooldowns started by anyone else stay. The
-- completions are locked after the caller's lock on the member row (member
-- first, the order Approve uses).
WITH mine AS (
    SELECT c.id, c.household_id,
           (c.member_id IS NULL OR c.member_id = sqlc.arg(member_id)::uuid) AS marker,
           (SELECT cm.member_id FROM quest_completion_members cm
            WHERE cm.household_id = c.household_id AND cm.completion_id = c.id
              AND cm.member_id <> sqlc.arg(member_id)::uuid
            ORDER BY cm.added_at ASC, cm.member_id ASC
            LIMIT 1) AS next_member
    FROM quest_completions c
    WHERE c.household_id = sqlc.arg(household_id) AND c.status = 'pending'
      AND (c.member_id = sqlc.arg(member_id)::uuid OR EXISTS (
          SELECT 1 FROM quest_completion_members cm
          WHERE cm.household_id = c.household_id AND cm.completion_id = c.id
            AND cm.member_id = sqlc.arg(member_id)::uuid
      ))
    FOR UPDATE OF c
), left_completions AS (
    DELETE FROM quest_completion_members cm
    USING mine m
    WHERE cm.household_id = m.household_id AND cm.completion_id = m.id
      AND cm.member_id = sqlc.arg(member_id)::uuid
    RETURNING cm.completion_id
), handed_over AS (
    UPDATE quest_completions hc
    SET member_id = m.next_member
    FROM mine m
    WHERE hc.id = m.id AND m.marker AND m.next_member IS NOT NULL
    RETURNING hc.id
), cancelled AS (
    UPDATE quest_completions cc
    SET status = 'cancelled'
    FROM mine m
    WHERE cc.id = m.id AND m.marker AND m.next_member IS NULL
    RETURNING cc.household_id, cc.quest_id, cc.done_at
)
UPDATE household_quests q
SET available_at = NOW(), updated_at = NOW()
FROM cancelled c
WHERE q.household_id = c.household_id AND q.id = c.quest_id
  AND q.completed_at = c.done_at AND q.available_at > NOW();

-- name: GetPendingCompletionMembers :one
-- The marker of a pending completion and every member it covers (earliest
-- recorded first), read without a lock so Approve can lock those members'
-- rows before it locks the completion (the order FamilyService.Remove uses).
-- A completion recorded before quest sharing has no member rows.
SELECT c.member_id,
       ARRAY(
           SELECT cm.member_id FROM quest_completion_members cm
           WHERE cm.household_id = c.household_id AND cm.completion_id = c.id
           ORDER BY cm.added_at, cm.member_id
       )::uuid[] AS member_ids
FROM quest_completions c
WHERE c.household_id = sqlc.arg(household_id) AND c.id = sqlc.arg(id)
  AND c.status = 'pending' AND c.member_id IS NOT NULL;

-- name: MarkQuestDone :one
-- A member on the quest marks it done: its holder, or any participant when
-- it is shared. In one statement the quest is released and starts its
-- cooldown from now (quest_next_available_at: local midnight in the
-- household's timezone, repeat_days days after today), one pending
-- completion recording the marker and the mark-done time is inserted for the
-- administrator, every member on the quest is recorded on that completion
-- (the lone marker when it is not shared, so n is 1), and the quest's
-- participants are cleared. Members keep their order: participants by
-- joined_at, the lone marker at now. NOW() is one instant per transaction, so
-- completed_at equals the completion's done_at. A quest the member is not on
-- matches no row and writes nothing, and neither does a quest whose claim has
-- run out (claim_expires_at at or before now, the end of the day it was
-- accepted), even before the sweep has released it: no completion, no streak
-- step. No points move here. The caller holds
-- the row locks of every member on the quest and then the quest's row lock
-- (LockHouseholdQuest, a statement of its own), so the participants recorded
-- and cleared here are the committed ones.
--
-- The same statement moves the member's streak for the quest (streaked):
-- the marker's count+1 when marked done by its expires_at, else 1, with a new expires_at
-- of local midnight starting the day after 2 x repeat_days days from today.
-- (prev_* still records the step it replaces; a reject no longer reads it
-- but recomputes the streak: RecomputeMemberQuestStreak.) The completion
-- stores the resulting count (streak_count), so its approval,
-- however late, pays the streak as of this mark-done. The streak write
-- touches only member_quest_streaks: never available_at or the cooldown.
WITH done AS (
    UPDATE household_quests q
    SET claimed_by = NULL, claimed_at = NULL, claim_expires_at = NULL,
        completed_at = NOW(),
        available_at = quest_next_available_at(NOW(), h.timezone, q.repeat_days),
        updated_at = NOW()
    FROM households h
    WHERE h.id = q.household_id
      AND q.household_id = sqlc.arg(household_id) AND q.id = sqlc.arg(id)
      AND q.claimed_by IS NOT NULL
      AND q.claim_expires_at > NOW()
      AND (q.claimed_by = sqlc.arg(member_id)::uuid OR EXISTS (
          SELECT 1 FROM household_quest_participants p
          WHERE p.household_id = q.household_id AND p.quest_id = q.id
            AND p.member_id = sqlc.arg(member_id)::uuid
      ))
    RETURNING q.*, h.timezone
), streaked AS (
    INSERT INTO member_quest_streaks (
        household_id, member_id, quest_id,
        count, last_done_at, expires_at,
        created_at, updated_at
    )
    SELECT
        d.household_id,
        sqlc.arg(member_id)::uuid,
        d.id,
        1,
        NOW(),
        quest_streak_expires_at(NOW(), d.timezone, d.repeat_days),
        NOW(),
        NOW()
    FROM done d
    ON CONFLICT (member_id, quest_id) DO UPDATE
    SET count = CASE
            WHEN NOW() <= member_quest_streaks.expires_at THEN member_quest_streaks.count + 1
            ELSE 1
        END,
        prev_count = member_quest_streaks.count,
        prev_last_done_at = member_quest_streaks.last_done_at,
        prev_expires_at = member_quest_streaks.expires_at,
        last_done_at = NOW(),
        expires_at = EXCLUDED.expires_at,
        updated_at = NOW()
    RETURNING quest_id, count
), recorded AS (
    INSERT INTO quest_completions (id, household_id, quest_id, member_id, done_at, status, streak_count)
    SELECT sqlc.arg(completion_id)::uuid, d.household_id, d.id, sqlc.arg(member_id)::uuid, NOW(), 'pending', s.count
    FROM done d
    JOIN streaked s ON s.quest_id = d.id
    RETURNING id, household_id
), on_quest AS (
    SELECT p.member_id, p.joined_at
    FROM done d
    JOIN household_quest_participants p ON p.household_id = d.household_id AND p.quest_id = d.id
    UNION ALL
    SELECT sqlc.arg(member_id)::uuid, NOW()
    FROM done d
    WHERE NOT EXISTS (
        SELECT 1 FROM household_quest_participants p
        WHERE p.household_id = d.household_id AND p.quest_id = d.id
          AND p.member_id = sqlc.arg(member_id)::uuid
    )
), recorded_members AS (
    INSERT INTO quest_completion_members (household_id, completion_id, member_id, added_at)
    SELECT r.household_id, r.id, o.member_id, o.joined_at
    FROM recorded r
    CROSS JOIN on_quest o
    RETURNING member_id
), cleared AS (
    DELETE FROM household_quest_participants p
    USING done d
    WHERE p.household_id = d.household_id AND p.quest_id = d.id
    RETURNING p.member_id
)
SELECT d.id, d.household_id, d.library_id, d.name, d.points, d.repeat_days, d.notes,
       d.claimed_by, d.claimed_at, d.done_at, d.completed_at, d.available_at,
       d.created_at, d.updated_at, d.shareable, d.teamwork_bonus, d.streak_bonus,
       d.min_age, d.max_age, d.claim_expires_at
FROM done d;

-- name: GetMemberQuestStreak :one
SELECT * FROM member_quest_streaks
WHERE household_id = sqlc.arg(household_id)
  AND member_id = sqlc.arg(member_id)
  AND quest_id = sqlc.arg(quest_id);

-- name: ListPendingCompletions :many
-- Completions waiting for approval, one row each, oldest mark-done first,
-- with the chore's name, current points and teamwork bonus, the member who
-- marked it done, and every member it covers (member_ids and member_names,
-- in the same order: earliest recorded first). A completion recorded before
-- quest sharing has no member rows; the caller reads its marker alone.
SELECT c.id, c.quest_id, q.name AS quest_name, q.points, q.teamwork_bonus, c.member_id,
       m.name AS member_name, c.done_at,
       ARRAY(
           SELECT cm.member_id FROM quest_completion_members cm
           JOIN family_members fm ON fm.household_id = cm.household_id AND fm.id = cm.member_id
           WHERE cm.household_id = c.household_id AND cm.completion_id = c.id
           ORDER BY cm.added_at, cm.member_id
       )::uuid[] AS member_ids,
       ARRAY(
           SELECT fm.name FROM quest_completion_members cm
           JOIN family_members fm ON fm.household_id = cm.household_id AND fm.id = cm.member_id
           WHERE cm.household_id = c.household_id AND cm.completion_id = c.id
           ORDER BY cm.added_at, cm.member_id
       )::text[] AS member_names
FROM quest_completions c
JOIN household_quests q ON q.household_id = c.household_id AND q.id = c.quest_id
JOIN family_members m ON m.household_id = c.household_id AND m.id = c.member_id
WHERE c.household_id = sqlc.arg(household_id) AND c.status = 'pending'
ORDER BY c.done_at ASC, c.id ASC;

-- name: ApproveCompletion :one
-- Approves one pending completion. The completion is locked first and its
-- status re-read under the lock, so of two concurrent approvals exactly one
-- gets a row. The quest is not touched: its cooldown already started at
-- mark-done. points and teamwork_bonus are the chore's as they stand now
-- (the CHECK keeps the bonus 0 once shareable is off); member_ids are the
-- members the completion covers, earliest recorded first, empty for a
-- completion recorded before quest sharing (its marker alone). member_id
-- must match the marker the caller already locked; the caller holds the
-- locks of every recorded member, so the member set cannot change under it.
-- quest_name is the chore's name, the reason in the points history.
-- streak_bonus is the chore's current streak bonus and streak_count the
-- marker's streak as fixed at this completion's mark-done (NULL for one
-- marked done before streaks existed); neither is recomputed from NOW().
WITH pending AS (
    SELECT pc.id, q.points, q.teamwork_bonus, q.name, q.streak_bonus
    FROM quest_completions pc
    JOIN household_quests q ON q.household_id = pc.household_id AND q.id = pc.quest_id
    WHERE pc.household_id = sqlc.arg(household_id) AND pc.id = sqlc.arg(id)
      AND pc.status = 'pending' AND pc.member_id = sqlc.arg(member_id)::uuid
    FOR UPDATE OF pc
), approved AS (
    UPDATE quest_completions c
    SET status = 'approved', approved_at = NOW()
    FROM pending p
    WHERE c.id = p.id AND c.status = 'pending'
    RETURNING c.id, c.household_id, c.quest_id, c.member_id, c.done_at, c.approved_at, p.points, p.teamwork_bonus,
              p.name AS quest_name, p.streak_bonus, c.streak_count
)
SELECT a.id, a.quest_id, a.member_id, a.done_at, a.approved_at, a.points, a.teamwork_bonus,
       a.quest_name, a.streak_bonus, a.streak_count,
       ARRAY(
           SELECT cm.member_id FROM quest_completion_members cm
           WHERE cm.household_id = a.household_id AND cm.completion_id = a.id
           ORDER BY cm.added_at, cm.member_id
       )::uuid[] AS member_ids
FROM approved a;

-- name: SetCompletionMemberAward :exec
-- Records what an approval credited one member of a completion: their split
-- plus any teamwork bonus. A completion recorded before quest sharing has no
-- member row, and this writes nothing for it.
UPDATE quest_completion_members
SET points_awarded = sqlc.arg(points_awarded)
WHERE household_id = sqlc.arg(household_id) AND completion_id = sqlc.arg(completion_id)
  AND member_id = sqlc.arg(member_id);

-- name: RejectCompletion :one
-- Rejects one pending completion: no points. If it started the quest's
-- running cooldown (the quest's completed_at is its done_at) and that
-- cooldown has not ended, the cooldown is cancelled so the chore is available
-- again right away. Other completions and the quest's holder are untouched.
WITH rejected AS (
    UPDATE quest_completions rc
    SET status = 'rejected'
    WHERE rc.household_id = sqlc.arg(household_id) AND rc.id = sqlc.arg(id)
      AND rc.status = 'pending'
    RETURNING rc.id, rc.household_id, rc.quest_id, rc.member_id, rc.done_at, rc.status
), reopened AS (
    UPDATE household_quests q
    SET available_at = NOW(), updated_at = NOW()
    FROM rejected r
    WHERE q.household_id = r.household_id AND q.id = r.quest_id
      AND q.completed_at = r.done_at AND q.available_at > NOW()
    RETURNING q.id
)
SELECT r.id, r.quest_id, r.member_id, r.done_at, r.status
FROM rejected r;

-- name: RecomputeMemberQuestStreak :exec
-- Rebuilds the member's streak for the quest from the completions that still
-- count: theirs, pending or approved, marked done since streaks existed
-- (streak_count set), in done_at order. It replaces undoing one stored step,
-- so a reject undoes its completion's step even when a later mark-done came
-- after it. The mark-done rule is replayed: a completion continues the
-- streak (count+1) when marked done by the previous one's expires_at, else
-- starts it again at 1; each expires_at is quest_streak_expires_at(done_at,
-- household timezone, repeat_days). member_quest_streaks gets the last
-- completion's count, done_at and expiry, or the row is deleted when no
-- completion counts. Pending completions get their replayed streak_count;
-- approved ones keep theirs, so bonuses already paid stay paid. Writes only
-- member_quest_streaks and quest_completions.streak_count: never the quest
-- or its cooldown. The caller holds the member row's lock.
WITH chain AS (
    SELECT c.id, c.status, c.done_at,
           quest_streak_expires_at(c.done_at, h.timezone, q.repeat_days) AS expires_at
    FROM quest_completions c
    JOIN household_quests q ON q.household_id = c.household_id AND q.id = c.quest_id
    JOIN households h ON h.id = c.household_id
    WHERE c.household_id = sqlc.arg(household_id)::uuid AND c.member_id = sqlc.arg(member_id)::uuid
      AND c.quest_id = sqlc.arg(quest_id)::uuid
      AND c.status IN ('pending', 'approved') AND c.streak_count IS NOT NULL
), stepped AS (
    SELECT ch.id, ch.status, ch.done_at, ch.expires_at,
           CASE WHEN ch.done_at <= LAG(ch.expires_at) OVER (ORDER BY ch.done_at, ch.id) THEN 0 ELSE 1 END AS starts
    FROM chain ch
), runs AS (
    SELECT st.id, st.status, st.done_at, st.expires_at,
           SUM(st.starts) OVER (ORDER BY st.done_at, st.id) AS run
    FROM stepped st
), counted AS (
    SELECT r.id, r.status, r.done_at, r.expires_at,
           (ROW_NUMBER() OVER (PARTITION BY r.run ORDER BY r.done_at, r.id))::int AS count
    FROM runs r
), latest AS (
    SELECT k.done_at, k.expires_at, k.count
    FROM counted k
    ORDER BY k.done_at DESC, k.id DESC
    LIMIT 1
), renumbered AS (
    UPDATE quest_completions c
    SET streak_count = k.count
    FROM counted k
    WHERE c.household_id = sqlc.arg(household_id)::uuid AND c.id = k.id
      AND k.status = 'pending' AND c.streak_count IS DISTINCT FROM k.count
    RETURNING c.id
), kept AS (
    INSERT INTO member_quest_streaks (
        household_id, member_id, quest_id,
        count, last_done_at, expires_at,
        created_at, updated_at
    )
    SELECT sqlc.arg(household_id)::uuid, sqlc.arg(member_id)::uuid, sqlc.arg(quest_id)::uuid,
           l.count, l.done_at, l.expires_at, NOW(), NOW()
    FROM latest l
    ON CONFLICT (member_id, quest_id) DO UPDATE
    SET count = EXCLUDED.count,
        last_done_at = EXCLUDED.last_done_at,
        expires_at = EXCLUDED.expires_at,
        prev_count = NULL, prev_last_done_at = NULL, prev_expires_at = NULL,
        updated_at = NOW()
    WHERE member_quest_streaks.household_id = EXCLUDED.household_id
    RETURNING quest_id
)
DELETE FROM member_quest_streaks s
WHERE s.household_id = sqlc.arg(household_id)::uuid AND s.member_id = sqlc.arg(member_id)::uuid
  AND s.quest_id = sqlc.arg(quest_id)::uuid
  AND NOT EXISTS (SELECT 1 FROM latest);
