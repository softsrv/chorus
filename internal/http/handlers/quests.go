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
	List(ctx context.Context, userID, householdID uuid.UUID, p app.QuestPage) ([]app.QuestView, bool, error)
	Get(ctx context.Context, userID, householdID, questID uuid.UUID) (app.QuestView, error)
	Create(ctx context.Context, userID, householdID uuid.UUID, in app.QuestInput, adminPin string) (app.QuestView, error)
	Update(ctx context.Context, userID, householdID, questID uuid.UUID, in app.QuestInput, adminPin string) (app.QuestView, error)
	Delete(ctx context.Context, userID, householdID, questID uuid.UUID, adminPin string) error
	CopyFromLibrary(ctx context.Context, userID, householdID, libraryID uuid.UUID, adminPin string) (app.QuestView, error)
	ListLibrary(ctx context.Context) ([]db.QuestLibrary, error)
	Accept(ctx context.Context, userID, householdID, questID uuid.UUID, act app.MemberAction) (app.QuestView, error)
	Abandon(ctx context.Context, userID, householdID, questID uuid.UUID, act app.MemberAction) (app.QuestView, error)
	MarkDone(ctx context.Context, userID, householdID, questID uuid.UUID, act app.MemberAction) (app.QuestView, error)
	Share(ctx context.Context, userID, householdID, questID uuid.UUID, act, target app.MemberAction) (app.QuestView, error)
	ListPending(ctx context.Context, userID, householdID uuid.UUID, adminPin string) ([]app.PendingCompletion, error)
	Approve(ctx context.Context, userID, householdID, completionID uuid.UUID, adminPin string) (app.Completion, db.FamilyMember, []app.ComboBonus, []app.ApprovedMember, error)
	Reject(ctx context.Context, userID, householdID, completionID uuid.UUID, adminPin string) (app.Completion, error)
}

// QuestHandler serves /api/v1/households/{householdID}/quests,
// /api/v1/households/{householdID}/quest-completions and
// /api/v1/quest-library. Writes carry the admin PIN in X-Admin-PIN.
type QuestHandler struct {
	svc questServicer
}

// NewQuestHandler constructs a QuestHandler.
func NewQuestHandler(svc questServicer) *QuestHandler {
	return &QuestHandler{svc: svc}
}

// questRequest is the body for create and update. Update is a full
// replacement (PUT). points, repeat_days, teamwork_bonus and streak_bonus stay
// raw so a wrong-typed value is reported as its own field error alongside the others.
// An absent shareable is false.
type questRequest struct {
	Name          string          `json:"name"`
	Points        json.RawMessage `json:"points"`
	RepeatDays    json.RawMessage `json:"repeat_days"`
	Notes         string          `json:"notes"`
	Shareable     *bool           `json:"shareable"`
	TeamworkBonus json.RawMessage `json:"teamwork_bonus"`
	StreakBonus   json.RawMessage `json:"streak_bonus"`
}

func (req questRequest) toInput() app.QuestInput {
	return app.QuestInput{
		Name:          req.Name,
		Points:        numberField(req.Points),
		RepeatDays:    numberField(req.RepeatDays),
		Notes:         req.Notes,
		Shareable:     req.Shareable != nil && *req.Shareable,
		TeamworkBonus: numberField(req.TeamworkBonus),
		StreakBonus:   numberField(req.StreakBonus),
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

// memberAction runs one of the acting member's quest actions (abandon, mark
// done) and answers the quest.
func (h *QuestHandler) memberAction(w http.ResponseWriter, r *http.Request,
	do func(ctx context.Context, userID, householdID, questID uuid.UUID, act app.MemberAction) (app.QuestView, error)) {
	hid, qid, ok := parseQuestIDs(w, r)
	if !ok {
		return
	}
	act, ok := decodeQuestAction(w, r)
	if !ok {
		return
	}
	quest, err := do(r.Context(), currentUser(r), hid, qid, act)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toQuest(quest))
}

// Abandon puts the acting member's quest back in the pool:
// {"member_id": "...", "pin": "1234"}.
func (h *QuestHandler) Abandon(w http.ResponseWriter, r *http.Request) {
	h.memberAction(w, r, h.svc.Abandon)
}

// MarkDone marks the acting member's quest done: the quest is released and
// rests until its cooldown ends, and a completion waits for approval:
// {"member_id": "...", "pin": "1234"}.
func (h *QuestHandler) MarkDone(w http.ResponseWriter, r *http.Request) {
	h.memberAction(w, r, h.svc.MarkDone)
}

// shareRequest is the body of a share: the sharing member and their PIN, and
// the member being shared with and theirs. Each PIN is omitted or empty when
// that member has none.
type shareRequest struct {
	MemberID       uuid.UUID `json:"member_id"`
	PIN            *string   `json:"pin"`
	TargetMemberID uuid.UUID `json:"target_member_id"`
	TargetPIN      *string   `json:"target_pin"`
}

// Share adds another household member to the acting member's shareable
// quest: {"member_id": "...", "pin": "1234", "target_member_id": "...",
// "target_pin": "5678"}. A missing member_id or target_member_id is a 422 on
// that field.
func (h *QuestHandler) Share(w http.ResponseWriter, r *http.Request) {
	hid, qid, ok := parseQuestIDs(w, r)
	if !ok {
		return
	}
	var req shareRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	fields := map[string]string{}
	if req.MemberID == uuid.Nil {
		fields["member_id"] = "is required"
	}
	if req.TargetMemberID == uuid.Nil {
		fields["target_member_id"] = "is required"
	}
	if len(fields) > 0 {
		writeError(w, r, &app.ValidationError{Fields: fields})
		return
	}
	act := app.MemberAction{MemberID: req.MemberID}
	if req.PIN != nil {
		act.PIN = *req.PIN
	}
	target := app.MemberAction{MemberID: req.TargetMemberID}
	if req.TargetPIN != nil {
		target.PIN = *req.TargetPIN
	}
	quest, err := h.svc.Share(r.Context(), currentUser(r), hid, qid, act, target)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toQuest(quest))
}

// Pending lists the completions waiting for approval, one per mark-done,
// oldest first. The admin PIN travels in X-Admin-PIN. None waiting answers
// {"items": []}, never null.
func (h *QuestHandler) Pending(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	rows, err := h.svc.ListPending(r.Context(), currentUser(r), hid, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[pendingCompletionJSON]{Items: mapSlice(rows, toPendingCompletion)})
}

func parseCompletionIDs(w http.ResponseWriter, r *http.Request) (hid, cid uuid.UUID, ok bool) {
	if hid, ok = pathUUID(w, r, "householdID"); !ok {
		return
	}
	cid, ok = pathUUID(w, r, "completionID")
	return
}

// decodeEmptyBody accepts no body or exactly {}: approve and reject carry
// everything in the path and the X-Admin-PIN header. Any field is a 400.
func decodeEmptyBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength == 0 {
		return true
	}
	var req struct{}
	return decodeJSON(w, r, &req)
}

// approveResponse is the approved completion, the member who marked it done
// with their new point total (bonuses included), every combo bonus the
// approval paid that member (combo_bonuses is [] when none), and every member
// it credited (members; one entry when it was not shared). streak_bonus is
// the streak bonus paid to the member who marked it done (0 when none) and
// streak_count their streak for the quest as fixed at mark-done (0 for a
// completion from before streaks).
type approveResponse struct {
	Completion   completionJSON       `json:"completion"`
	Member       familyMemberJSON     `json:"member"`
	ComboBonuses []comboAwardJSON     `json:"combo_bonuses"`
	StreakBonus  int32                `json:"streak_bonus"`
	StreakCount  int32                `json:"streak_count"`
	Members      []approvedMemberJSON `json:"members"`
}

// approvedMemberJSON is one member an approval credited: the member with
// their new point total, their split of the chore's points, and the teamwork
// bonus paid on top (0 when the completion was not shared).
type approvedMemberJSON struct {
	Member        familyMemberJSON `json:"member"`
	PointsAwarded int32            `json:"points_awarded"`
	TeamworkBonus int32            `json:"teamwork_bonus"`
}

func toApprovedMember(m app.ApprovedMember) approvedMemberJSON {
	return approvedMemberJSON{Member: toFamilyMember(m.Member), PointsAwarded: m.PointsAwarded, TeamworkBonus: m.TeamworkBonus}
}

// completionResponse wraps a rejected completion.
type completionResponse struct {
	Completion completionJSON `json:"completion"`
}

// Approve approves a pending completion and credits every member it covers
// their split of the chore's points (plus the teamwork bonus when shared) and
// any combo bonuses it finishes for them, and the streak bonus to the member
// who marked it done.
// The admin PIN travels in X-Admin-PIN; the body is empty or {}.
func (h *QuestHandler) Approve(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := parseCompletionIDs(w, r)
	if !ok || !decodeEmptyBody(w, r) {
		return
	}
	completion, member, bonuses, members, err := h.svc.Approve(r.Context(), currentUser(r), hid, cid, r.Header.Get("X-Admin-PIN"))
	var streak app.StreakBonus
	for _, am := range members {
		if am.Member.ID == member.ID {
			streak = am.Streak
		}
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, approveResponse{
		Completion:   toCompletion(completion),
		Member:       toFamilyMember(member),
		ComboBonuses: mapSlice(bonuses, toComboAward),
		StreakBonus:  streak.Points,
		StreakCount:  streak.Count,
		Members:      mapSlice(members, toApprovedMember),
	})
}

// Reject rejects a pending completion with no points. The admin PIN travels
// in X-Admin-PIN; the body is empty or {}.
func (h *QuestHandler) Reject(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := parseCompletionIDs(w, r)
	if !ok || !decodeEmptyBody(w, r) {
		return
	}
	completion, err := h.svc.Reject(r.Context(), currentUser(r), hid, cid, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, completionResponse{Completion: toCompletion(completion)})
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
