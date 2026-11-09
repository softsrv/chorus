package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/respond"
)

// defaultPageSize is the page size of a list request with no ?limit=.
const defaultPageSize = 50

type questServicer interface {
	List(ctx context.Context, userID, householdID uuid.UUID, p app.QuestPage) ([]db.HouseholdQuest, bool, error)
	Get(ctx context.Context, userID, householdID, questID uuid.UUID) (db.HouseholdQuest, error)
	Create(ctx context.Context, userID, householdID uuid.UUID, in app.QuestInput, adminPin string) (db.HouseholdQuest, error)
	Update(ctx context.Context, userID, householdID, questID uuid.UUID, in app.QuestInput, adminPin string) (db.HouseholdQuest, error)
	Delete(ctx context.Context, userID, householdID, questID uuid.UUID, adminPin string) error
	CopyFromLibrary(ctx context.Context, userID, householdID, libraryID uuid.UUID, adminPin string) (db.HouseholdQuest, error)
	ListLibrary(ctx context.Context) ([]db.QuestLibrary, error)
	Accept(ctx context.Context, userID, householdID, questID uuid.UUID, act app.MemberAction) (db.HouseholdQuest, error)
}

// QuestHandler serves /api/v1/households/{householdID}/quests and
// /api/v1/quest-library. Writes carry the admin PIN in X-Admin-PIN.
type QuestHandler struct {
	svc questServicer
}

// NewQuestHandler constructs a QuestHandler.
func NewQuestHandler(svc questServicer) *QuestHandler {
	return &QuestHandler{svc: svc}
}

// questRequest is the body for create and update. Update is a full
// replacement (PUT). points and repeat_days stay raw so a wrong-typed value
// is reported as its own field error alongside the others.
type questRequest struct {
	Name       string          `json:"name"`
	Points     json.RawMessage `json:"points"`
	RepeatDays json.RawMessage `json:"repeat_days"`
	Notes      string          `json:"notes"`
}

func (req questRequest) toInput() app.QuestInput {
	return app.QuestInput{
		Name:       req.Name,
		Points:     numberField(req.Points),
		RepeatDays: numberField(req.RepeatDays),
		Notes:      req.Notes,
	}
}

// numberField reads an optional JSON integer: absent or null is unset; any
// other value that is not a whole number is Invalid.
func numberField(raw json.RawMessage) app.NumberField {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return app.NumberField{}
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return app.NumberField{Set: true, Invalid: true}
	}
	return app.NumberField{Set: true, Value: n}
}

type fromLibraryRequest struct {
	LibraryID uuid.UUID `json:"library_id"`
}

// questListResponse adds a pagination cursor: next_offset is null on the
// last page.
type questListResponse struct {
	Items      []questJSON `json:"items"`
	NextOffset *int        `json:"next_offset"`
}

func parseQuestIDs(w http.ResponseWriter, r *http.Request) (hid, qid uuid.UUID, ok bool) {
	if hid, ok = pathUUID(w, r, "householdID"); !ok {
		return
	}
	qid, ok = pathUUID(w, r, "questID")
	return
}

// List supports ?limit= / ?offset= pagination. An empty household answers
// {"items": []}, never null.
func (h *QuestHandler) List(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var p app.QuestPage
	var err error
	if p.Limit, err = queryInt(r, "limit", defaultPageSize); err != nil {
		writeError(w, r, err)
		return
	}
	if p.Offset, err = queryInt(r, "offset", 0); err != nil {
		writeError(w, r, err)
		return
	}
	quests, more, err := h.svc.List(r.Context(), currentUser(r), hid, p)
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := questListResponse{Items: mapSlice(quests, toQuest)}
	if more {
		next := p.Offset + len(quests)
		resp.NextOffset = &next
	}
	respond.JSON(w, http.StatusOK, resp)
}

// Get returns one quest.
func (h *QuestHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid, qid, ok := parseQuestIDs(w, r)
	if !ok {
		return
	}
	quest, err := h.svc.Get(r.Context(), currentUser(r), hid, qid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toQuest(quest))
}

// Create adds a quest.
func (h *QuestHandler) Create(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req questRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	quest, err := h.svc.Create(r.Context(), currentUser(r), hid, req.toInput(), r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toQuest(quest))
}

// Update replaces a quest's editable fields.
func (h *QuestHandler) Update(w http.ResponseWriter, r *http.Request) {
	hid, qid, ok := parseQuestIDs(w, r)
	if !ok {
		return
	}
	var req questRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	quest, err := h.svc.Update(r.Context(), currentUser(r), hid, qid, req.toInput(), r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toQuest(quest))
}

// Delete removes a quest.
func (h *QuestHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid, qid, ok := parseQuestIDs(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), currentUser(r), hid, qid, r.Header.Get("X-Admin-PIN")); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

// CopyFromLibrary adds a copy of a library quest: {"library_id": "..."}.
func (h *QuestHandler) CopyFromLibrary(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req fromLibraryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.LibraryID == uuid.Nil {
		writeError(w, r, &app.ValidationError{Fields: map[string]string{"library_id": "is required"}})
		return
	}
	quest, err := h.svc.CopyFromLibrary(r.Context(), currentUser(r), hid, req.LibraryID, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toQuest(quest))
}

// questActionRequest is the body of a member's quest action: the acting
// member and, when the member has one, their PIN.
type questActionRequest struct {
	MemberID uuid.UUID `json:"member_id"`
	PIN      *string   `json:"pin"`
}

// decodeQuestAction reads a strict questActionRequest; a missing member_id
// is a 422 on that field.
func decodeQuestAction(w http.ResponseWriter, r *http.Request) (app.MemberAction, bool) {
	var req questActionRequest
	if !decodeJSON(w, r, &req) {
		return app.MemberAction{}, false
	}
	if req.MemberID == uuid.Nil {
		writeError(w, r, &app.ValidationError{Fields: map[string]string{"member_id": "is required"}})
		return app.MemberAction{}, false
	}
	act := app.MemberAction{MemberID: req.MemberID}
	if req.PIN != nil {
		act.PIN = *req.PIN
	}
	return act, true
}

// Accept assigns an available quest to the acting member:
// {"member_id": "...", "pin": "1234"}.
func (h *QuestHandler) Accept(w http.ResponseWriter, r *http.Request) {
	hid, qid, ok := parseQuestIDs(w, r)
	if !ok {
		return
	}
	act, ok := decodeQuestAction(w, r)
	if !ok {
		return
	}
	quest, err := h.svc.Accept(r.Context(), currentUser(r), hid, qid, act)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toQuest(quest))
}

// Library lists the global quest library.
func (h *QuestHandler) Library(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListLibrary(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[libraryQuestJSON]{Items: mapSlice(items, toLibraryQuest)})
}
