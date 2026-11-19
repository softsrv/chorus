package handlers

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
)

// These types define the API's wire format. They are deliberately separate
// from the sqlc models so schema changes can't silently change the API (or
// leak columns such as password_hash).

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type userJSON struct {
	ID            uuid.UUID `json:"id"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	EmailVerified bool      `json:"email_verified"`
	// HasPassword is false for passwordless (Google-only) accounts, which
	// can't change a password or confirm destructive actions with one.
	HasPassword bool      `json:"has_password"`
	CreatedAt   time.Time `json:"created_at"`
}

func toUser(u db.User) userJSON {
	return userJSON{
		ID:            u.ID,
		Email:         u.Email,
		DisplayName:   u.DisplayName,
		EmailVerified: u.EmailVerified,
		HasPassword:   u.PasswordHash.Valid && u.PasswordHash.String != "",
		CreatedAt:     u.CreatedAt.Time.UTC(),
	}
}

type tokenJSON struct {
	TokenType             string    `json:"token_type"`
	AccessToken           string    `json:"access_token"`
	AccessTokenExpiresAt  time.Time `json:"access_token_expires_at"`
	RefreshToken          string    `json:"refresh_token"`
	RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
	User                  userJSON  `json:"user"`
}

func toTokens(t app.TokenResult) tokenJSON {
	return tokenJSON{
		TokenType:             "Bearer",
		AccessToken:           t.AccessToken,
		AccessTokenExpiresAt:  t.AccessTokenExpiry.UTC(),
		RefreshToken:          t.RefreshToken,
		RefreshTokenExpiresAt: t.RefreshTokenExpiry.UTC(),
		User:                  toUser(t.User),
	}
}

type sessionJSON struct {
	ID         uuid.UUID `json:"id"`
	DeviceName string    `json:"device_name"`
	IPAddress  string    `json:"ip_address"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

func toSession(rt db.RefreshToken, current uuid.UUID) sessionJSON {
	s := sessionJSON{
		ID:         rt.ID,
		DeviceName: rt.DeviceName.String,
		CreatedAt:  rt.CreatedAt.Time.UTC(),
		LastUsedAt: rt.LastUsedAt.Time.UTC(),
		ExpiresAt:  rt.ExpiresAt.Time.UTC(),
		Current:    rt.ID == current,
	}
	if rt.IpAddress != nil {
		s.IPAddress = rt.IpAddress.String()
	}
	return s
}

type householdJSON struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Timezone  string    `json:"timezone"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toHousehold(h app.Household) householdJSON {
	return householdJSON{
		ID:        h.ID,
		Name:      h.Name,
		Timezone:  h.Timezone,
		Role:      h.Role,
		CreatedAt: h.CreatedAt.Time.UTC(),
		UpdatedAt: h.UpdatedAt.Time.UTC(),
	}
}

type familyMemberJSON struct {
	ID     uuid.UUID `json:"id"`
	Name   string    `json:"name"`
	HasPIN bool      `json:"has_pin"`
	Points int32     `json:"points"`
}

func toFamilyMember(m db.FamilyMember) familyMemberJSON {
	return familyMemberJSON{
		ID:     m.ID,
		Name:   m.Name,
		HasPIN: m.PinHash.Valid && m.PinHash.String != "",
		Points: m.Points,
	}
}

// questJSON is a household quest. The lifecycle fields (claimed_by,
// claimed_at, claim_expires_at, completed_at, available_at) are read-only:
// they change through quest actions and, for a held quest, through the
// expiry sweep. claim_expires_at is the household-local midnight that ends
// the day the quest was accepted; if it is not marked done by then, the
// sweep (not a member action) clears claimed_by, claimed_at and
// claim_expires_at and the quest is back in the pool. completed_at is when it was last marked done and
// available_at when its cooldown from that moment ends. available says the
// quest is in the pool; awaiting_approval says at least one of its
// completions waits for the administrator to approve or reject it.
type questJSON struct {
	ID               uuid.UUID   `json:"id"`
	HouseholdID      uuid.UUID   `json:"household_id"`
	LibraryID        *uuid.UUID  `json:"library_id"`
	Name             string      `json:"name"`
	Points           int32       `json:"points"`
	RepeatDays       int32       `json:"repeat_days"`
	Notes            string      `json:"notes"`
	StreakBonus      int32       `json:"streak_bonus"`
	Available        bool        `json:"available"`
	ClaimedBy        *uuid.UUID  `json:"claimed_by"`
	ClaimedAt        *time.Time  `json:"claimed_at"`
	ClaimExpiresAt   *time.Time  `json:"claim_expires_at"`
	AwaitingApproval bool        `json:"awaiting_approval"`
	CompletedAt      *time.Time  `json:"completed_at"`
	AvailableAt      *time.Time  `json:"available_at"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
	Shareable        bool        `json:"shareable"`
	TeamworkBonus    int32       `json:"teamwork_bonus"`
	Shared           bool        `json:"shared"`
	Participants     []uuid.UUID `json:"participants"`
	MinAge           *int32      `json:"min_age"`
	MaxAge           *int32      `json:"max_age"`
	// IneligibleMemberIDs are the household's members outside the quest's
	// age range today; never a birthday or an age.
	IneligibleMemberIDs []uuid.UUID `json:"ineligible_member_ids"`
}

// toQuest maps a quest; awaiting_approval comes only from the service's
// pending-completion flag. participants is [] for a quest that is not shared.
func toQuest(v app.QuestView) questJSON {
	q := v.HouseholdQuest
	participants := v.Participants
	if participants == nil {
		participants = []uuid.UUID{}
	}
	ineligible := v.IneligibleMemberIDs
	if ineligible == nil {
		ineligible = []uuid.UUID{}
	}
	return questJSON{
		ID:               q.ID,
		HouseholdID:      q.HouseholdID,
		LibraryID:        q.LibraryID,
		Name:             q.Name,
		Points:           q.Points,
		RepeatDays:       q.RepeatDays,
		Notes:            q.Notes,
		StreakBonus:      q.StreakBonus,
		Available:        app.QuestAvailable(q, time.Now()),
		ClaimedBy:        q.ClaimedBy,
		ClaimedAt:        timePtr(q.ClaimedAt),
		ClaimExpiresAt:   timePtr(q.ClaimExpiresAt),
		AwaitingApproval: v.AwaitingApproval,
		CompletedAt:      timePtr(q.CompletedAt),
		AvailableAt:      timePtr(q.AvailableAt),
		CreatedAt:        q.CreatedAt.Time.UTC(),
		UpdatedAt:        q.UpdatedAt.Time.UTC(),
		Shareable:        q.Shareable,
		TeamworkBonus:    q.TeamworkBonus,
		Shared:           v.Shared(),
		Participants:     participants,
		MinAge:           int4Ptr(q.MinAge),
		MaxAge:           int4Ptr(q.MaxAge),

		IneligibleMemberIDs: ineligible,
	}
}

// questScheduleJSON is one scheduled quest; start_date is the
// household-local date it is assigned on, YYYY-MM-DD.
type questScheduleJSON struct {
	ID         uuid.UUID `json:"id"`
	QuestID    uuid.UUID `json:"quest_id"`
	QuestName  string    `json:"quest_name"`
	MemberID   uuid.UUID `json:"member_id"`
	MemberName string    `json:"member_name"`
	StartDate  string    `json:"start_date"`
	Repeat     bool      `json:"repeat"`
	CreatedAt  time.Time `json:"created_at"`
}

func toQuestSchedule(s app.QuestSchedule) questScheduleJSON {
	return questScheduleJSON{
		ID:         s.ID,
		QuestID:    s.QuestID,
		QuestName:  s.QuestName,
		MemberID:   s.MemberID,
		MemberName: s.MemberName,
		StartDate:  s.StartDate,
		Repeat:     s.Repeat,
		CreatedAt:  s.CreatedAt.UTC(),
	}
}

// upcomingQuestJSON is one of a member's upcoming scheduled quests;
// start_date and next_date are household-local dates, YYYY-MM-DD.
type upcomingQuestJSON struct {
	ID        uuid.UUID `json:"id"`
	QuestID   uuid.UUID `json:"quest_id"`
	QuestName string    `json:"quest_name"`
	StartDate string    `json:"start_date"`
	NextDate  string    `json:"next_date"`
	Repeat    bool      `json:"repeat"`
}

func toUpcomingQuest(u app.UpcomingQuest) upcomingQuestJSON {
	return upcomingQuestJSON{
		ID:        u.ID,
		QuestID:   u.QuestID,
		QuestName: u.QuestName,
		StartDate: u.StartDate,
		NextDate:  u.NextDate,
		Repeat:    u.Repeat,
	}
}

// int4Ptr is nil for a NULL column, so it marshals as null.
func int4Ptr(n pgtype.Int4) *int32 {
	if !n.Valid {
		return nil
	}
	return &n.Int32
}

// completionJSON is one time a member marked a chore done. approved_at is
// present once approved; status is pending, approved, rejected or cancelled.
// member_id is null once the member has been removed. member_ids, present on
// an approved completion, lists every member it credited, earliest first.
type completionJSON struct {
	ID         uuid.UUID   `json:"id"`
	QuestID    uuid.UUID   `json:"quest_id"`
	MemberID   *uuid.UUID  `json:"member_id"`
	MemberIDs  []uuid.UUID `json:"member_ids,omitempty"`
	DoneAt     time.Time   `json:"done_at"`
	ApprovedAt *time.Time  `json:"approved_at,omitempty"`
	Status     string      `json:"status"`
}

func toCompletion(c app.Completion) completionJSON {
	return completionJSON{
		ID:         c.ID,
		QuestID:    c.QuestID,
		MemberID:   c.MemberID,
		MemberIDs:  c.MemberIDs,
		DoneAt:     c.DoneAt,
		ApprovedAt: c.ApprovedAt,
		Status:     c.Status,
	}
}

// pendingCompletionJSON is a completion waiting for approval, with the
// chore's name, points and teamwork bonus, the member who marked it done
// (member_id, member_name) and every member it covers with their split
// (members; one entry when it is not shared). teamwork_bonus is what each
// member earns on top of their split, 0 when it is not shared.
type pendingCompletionJSON struct {
	CompletionID  uuid.UUID           `json:"completion_id"`
	QuestID       uuid.UUID           `json:"quest_id"`
	Name          string              `json:"name"`
	Points        int32               `json:"points"`
	TeamworkBonus int32               `json:"teamwork_bonus"`
	MemberID      uuid.UUID           `json:"member_id"`
	MemberName    string              `json:"member_name"`
	DoneAt        time.Time           `json:"done_at"`
	Members       []pendingMemberJSON `json:"members"`
}

// pendingMemberJSON is one member of a pending completion and the points
// their split will credit.
type pendingMemberJSON struct {
	MemberID   uuid.UUID `json:"member_id"`
	MemberName string    `json:"member_name"`
	Points     int32     `json:"points"`
}

func toPendingCompletion(p app.PendingCompletion) pendingCompletionJSON {
	members := make([]pendingMemberJSON, len(p.Members))
	for i, m := range p.Members {
		members[i] = pendingMemberJSON{MemberID: m.MemberID, MemberName: m.Name, Points: m.Award}
	}
	return pendingCompletionJSON{
		CompletionID:  p.ID,
		QuestID:       p.QuestID,
		Name:          p.QuestName,
		Points:        p.Points,
		TeamworkBonus: p.TeamworkBonus,
		MemberID:      p.MemberID,
		MemberName:    p.MemberName,
		DoneAt:        p.DoneAt,
		Members:       members,
	}
}

// libraryQuestJSON is a quest of the global, read-only library. streak_bonus
// is the default a household copy starts with.
type libraryQuestJSON struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Points      int32     `json:"points"`
	RepeatDays  int32     `json:"repeat_days"`
	Notes       string    `json:"notes"`
	StreakBonus int32     `json:"streak_bonus"`
}

func toLibraryQuest(q db.QuestLibrary) libraryQuestJSON {
	return libraryQuestJSON{ID: q.ID, Name: q.Name, Points: q.Points, RepeatDays: q.RepeatDays, Notes: q.Notes, StreakBonus: q.StreakBonus}
}

type pointsAwardJSON struct {
	ID       uuid.UUID `json:"id"`
	Kind     string    `json:"kind"`
	Reason   string    `json:"reason"`
	Points   int32     `json:"points"`
	EarnedAt time.Time `json:"earned_at"`
}

func toPointsAward(a app.PointsAward) pointsAwardJSON {
	return pointsAwardJSON{
		ID:       a.ID,
		Kind:     a.Kind,
		Reason:   a.Reason,
		Points:   a.Points,
		EarnedAt: a.EarnedAt,
	}
}

// rewardJSON is a household reward. library_id is set when it was adopted
// from the library.
type rewardJSON struct {
	ID          uuid.UUID  `json:"id"`
	HouseholdID uuid.UUID  `json:"household_id"`
	LibraryID   *uuid.UUID `json:"library_id"`
	Description string     `json:"description"`
	Cost        int32      `json:"cost"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func toReward(r db.HouseholdReward) rewardJSON {
	return rewardJSON{
		ID:          r.ID,
		HouseholdID: r.HouseholdID,
		LibraryID:   r.LibraryID,
		Description: r.Description,
		Cost:        r.Cost,
		CreatedAt:   r.CreatedAt.Time.UTC(),
		UpdatedAt:   r.UpdatedAt.Time.UTC(),
	}
}

// libraryRewardJSON is a reward of the global, read-only library.
type libraryRewardJSON struct {
	ID          uuid.UUID `json:"id"`
	Description string    `json:"description"`
	Cost        int32     `json:"cost"`
}

func toLibraryReward(r db.RewardLibrary) libraryRewardJSON {
	return libraryRewardJSON{ID: r.ID, Description: r.Description, Cost: r.Cost}
}

// rewardRedemptionJSON is a member's request to redeem a reward. description
// and cost are snapshotted at request time; reward_id is null once the
// reward was deleted. status is pending, approved, rejected or cancelled;
// decided_at is null while pending.
type rewardRedemptionJSON struct {
	ID          uuid.UUID  `json:"id"`
	MemberID    uuid.UUID  `json:"member_id"`
	RewardID    *uuid.UUID `json:"reward_id"`
	Description string     `json:"description"`
	Cost        int32      `json:"cost"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	DecidedAt   *time.Time `json:"decided_at"`
}

func toRewardRedemption(r db.RewardRedemption) rewardRedemptionJSON {
	out := rewardRedemptionJSON{
		ID:          r.ID,
		MemberID:    r.MemberID,
		RewardID:    r.RewardID,
		Description: r.Description,
		Cost:        r.Cost,
		Status:      r.Status,
		RequestedAt: r.RequestedAt.Time.UTC(),
	}
	if r.DecidedAt.Valid {
		t := r.DecidedAt.Time.UTC()
		out.DecidedAt = &t
	}
	return out
}

// pendingRedemptionJSON is a request in the admin's pending list: the
// redemption plus the requesting member's name.
type pendingRedemptionJSON struct {
	rewardRedemptionJSON
	MemberName string `json:"member_name"`
}

func toPendingRedemption(r db.ListPendingRewardRedemptionsRow) pendingRedemptionJSON {
	return pendingRedemptionJSON{
		rewardRedemptionJSON: toRewardRedemption(db.RewardRedemption{
			ID: r.ID, HouseholdID: r.HouseholdID, MemberID: r.MemberID, RewardID: r.RewardID,
			Description: r.Description, Cost: r.Cost, Status: r.Status,
			RequestedAt: r.RequestedAt, DecidedAt: r.DecidedAt,
		}),
		MemberName: r.MemberName,
	}
}

// comboJSON is the wire representation of a household combo.
type comboJSON struct {
	ID          uuid.UUID   `json:"id"`
	Name        string      `json:"name"`
	BonusPoints int32       `json:"bonus_points"`
	QuestIDs    []uuid.UUID `json:"quest_ids"`
}

func toCombo(c app.Combo) comboJSON {
	qids := c.QuestIDs
	if qids == nil {
		qids = []uuid.UUID{}
	}
	return comboJSON{
		ID:          c.ID,
		Name:        c.Name,
		BonusPoints: c.BonusPoints,
		QuestIDs:    qids,
	}
}

// comboAwardJSON is one combo bonus paid by an approval.
type comboAwardJSON struct {
	ComboID     uuid.UUID `json:"combo_id"`
	Name        string    `json:"name"`
	BonusPoints int32     `json:"bonus_points"`
}

func toComboAward(b app.ComboBonus) comboAwardJSON {
	return comboAwardJSON{ComboID: b.ComboID, Name: b.Name, BonusPoints: b.BonusPoints}
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, len(in))
	for i, v := range in {
		out[i] = f(v)
	}
	return out
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}
