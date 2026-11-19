package handlers

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

type questScheduleServicer interface {
	Schedule(ctx context.Context, userID, householdID uuid.UUID, in app.ScheduleInput, adminPin string) (app.QuestSchedule, error)
	List(ctx context.Context, userID, householdID uuid.UUID, adminPin string) ([]app.QuestSchedule, error)
	Cancel(ctx context.Context, userID, householdID, scheduleID uuid.UUID, adminPin string) error
	Upcoming(ctx context.Context, userID, householdID, memberID uuid.UUID) ([]app.UpcomingQuest, error)
}

// QuestScheduleHandler serves /api/v1/households/{householdID}/quest-schedules.
// Every route is admin-only and carries the admin PIN in X-Admin-PIN once
// one is set.
type QuestScheduleHandler struct {
	svc questScheduleServicer
}

// NewQuestScheduleHandler constructs a QuestScheduleHandler.
func NewQuestScheduleHandler(svc questScheduleServicer) *QuestScheduleHandler {
	return &QuestScheduleHandler{svc: svc}
}

type questScheduleRequest struct {
	QuestID  uuid.UUID `json:"quest_id"`
	MemberID uuid.UUID `json:"member_id"`
	Date     string    `json:"date"`
}

// Create schedules a quest for a member: {"quest_id", "member_id", "date"}
// with date a household-local YYYY-MM-DD. 201 with the schedule.
func (h *QuestScheduleHandler) Create(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req questScheduleRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	s, err := h.svc.Schedule(r.Context(), currentUser(r), hid, app.ScheduleInput{
		QuestID:  req.QuestID,
		MemberID: req.MemberID,
		Date:     req.Date,
	}, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toQuestSchedule(s))
}

// List returns the household's schedules, soonest first. None answers
// {"items": []}, never null.
func (h *QuestScheduleHandler) List(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	schedules, err := h.svc.List(r.Context(), currentUser(r), hid, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[questScheduleJSON]{Items: mapSlice(schedules, toQuestSchedule)})
}

// Upcoming returns one member's upcoming scheduled quests, soonest next_date
// first. Any household member may read it (no PIN); a non-member, or a member
// not in the household, is 404. None answers {"items": []}, never null.
func (h *QuestScheduleHandler) Upcoming(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	mid, ok := pathUUID(w, r, "memberID")
	if !ok {
		return
	}
	upcoming, err := h.svc.Upcoming(r.Context(), currentUser(r), hid, mid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[upcomingQuestJSON]{Items: mapSlice(upcoming, toUpcomingQuest)})
}

// Delete removes one schedule. 204; 404 for a schedule not in the household.
func (h *QuestScheduleHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	sid, ok := pathUUID(w, r, "scheduleID")
	if !ok {
		return
	}
	if err := h.svc.Cancel(r.Context(), currentUser(r), hid, sid, r.Header.Get("X-Admin-PIN")); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}
