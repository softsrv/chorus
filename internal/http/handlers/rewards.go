package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/respond"
)

type rewardServicer interface {
	List(ctx context.Context, userID, householdID uuid.UUID) ([]db.HouseholdReward, error)
	Get(ctx context.Context, userID, householdID, rewardID uuid.UUID) (db.HouseholdReward, error)
	Create(ctx context.Context, userID, householdID uuid.UUID, in app.RewardInput, adminPin string) (db.HouseholdReward, error)
	Update(ctx context.Context, userID, householdID, rewardID uuid.UUID, in app.RewardInput, adminPin string) (db.HouseholdReward, error)
	Delete(ctx context.Context, userID, householdID, rewardID uuid.UUID, adminPin string) error
	CopyFromLibrary(ctx context.Context, userID, householdID, libraryID uuid.UUID, adminPin string) (db.HouseholdReward, error)
	ListLibrary(ctx context.Context) ([]db.RewardLibrary, error)
	RequestRedemption(ctx context.Context, userID, householdID, rewardID uuid.UUID, act app.MemberAction) (db.RewardRedemption, bool, error)
	ListMemberPending(ctx context.Context, userID, householdID, memberID uuid.UUID, memberPin, adminPin string) ([]db.RewardRedemption, error)
	RewardHistory(ctx context.Context, userID, householdID, memberID uuid.UUID, memberPin, adminPin string, limit, offset int32) ([]db.RewardRedemption, error)
	ListPending(ctx context.Context, userID, householdID uuid.UUID, adminPin string) ([]db.ListPendingRewardRedemptionsRow, error)
	Approve(ctx context.Context, userID, householdID, redemptionID uuid.UUID, adminPin string) (db.RewardRedemption, db.FamilyMember, error)
	Reject(ctx context.Context, userID, householdID, redemptionID uuid.UUID, adminPin string) (db.RewardRedemption, error)
}

// RewardHandler serves /api/v1/households/{householdID}/rewards,
// /api/v1/households/{householdID}/reward-redemptions and
// /api/v1/reward-library. Reward writes, the pending list, approve and reject
// carry the admin PIN in X-Admin-PIN. A member redeems with member_id and pin
// in the body; the request waits for the admin and moves no points until it
// is approved.
type RewardHandler struct {
	svc rewardServicer
}

// NewRewardHandler constructs a RewardHandler.
func NewRewardHandler(svc rewardServicer) *RewardHandler {
	return &RewardHandler{svc: svc}
}

// rewardRequest is the body for create and update. Update is a full
// replacement (PUT). cost stays raw so a non-numeric or fractional value is
// reported as a 'cost' field error alongside the others.
type rewardRequest struct {
	Description string          `json:"description"`
	Cost        json.RawMessage `json:"cost"`
}

func (req rewardRequest) toInput() app.RewardInput {
	return app.RewardInput{Description: req.Description, Cost: numberField(req.Cost)}
}

func parseRewardIDs(w http.ResponseWriter, r *http.Request) (hid, rid uuid.UUID, ok bool) {
	if hid, ok = pathUUID(w, r, "householdID"); !ok {
		return
	}
	rid, ok = pathUUID(w, r, "rewardID")
	return
}

// List returns every reward of the household. An empty household answers
// {"items": []}, never null.
func (h *RewardHandler) List(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	rewards, err := h.svc.List(r.Context(), currentUser(r), hid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[rewardJSON]{Items: mapSlice(rewards, toReward)})
}

// Get returns one reward.
func (h *RewardHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid, rid, ok := parseRewardIDs(w, r)
	if !ok {
		return
	}
	reward, err := h.svc.Get(r.Context(), currentUser(r), hid, rid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toReward(reward))
}

// Create adds a reward: {"description": "...", "cost": 100}.
func (h *RewardHandler) Create(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req rewardRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reward, err := h.svc.Create(r.Context(), currentUser(r), hid, req.toInput(), r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toReward(reward))
}

// Update replaces a reward's description and cost.
func (h *RewardHandler) Update(w http.ResponseWriter, r *http.Request) {
	hid, rid, ok := parseRewardIDs(w, r)
	if !ok {
		return
	}
	var req rewardRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	reward, err := h.svc.Update(r.Context(), currentUser(r), hid, rid, req.toInput(), r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toReward(reward))
}

// Delete removes a reward.
func (h *RewardHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid, rid, ok := parseRewardIDs(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), currentUser(r), hid, rid, r.Header.Get("X-Admin-PIN")); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

// CopyFromLibrary adds a copy of a library reward: {"library_id": "..."}.
func (h *RewardHandler) CopyFromLibrary(w http.ResponseWriter, r *http.Request) {
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
	reward, err := h.svc.CopyFromLibrary(r.Context(), currentUser(r), hid, req.LibraryID, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toReward(reward))
}

// Library lists the global reward library.
func (h *RewardHandler) Library(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListLibrary(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[libraryRewardJSON]{Items: mapSlice(items, toLibraryReward)})
}

// Redeem records the acting member's request to redeem a reward:
// {"member_id": "...", "pin": "1234"}. A new request answers 201, a reward
// the member already has pending answers 200 with that request. A member
// short of points is a 422 on points.
func (h *RewardHandler) Redeem(w http.ResponseWriter, r *http.Request) {
	hid, rid, ok := parseRewardIDs(w, r)
	if !ok {
		return
	}
	act, ok := decodeQuestAction(w, r)
	if !ok {
		return
	}
	red, created, err := h.svc.RequestRedemption(r.Context(), currentUser(r), hid, rid, act)
	if err != nil {
		writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respond.JSON(w, status, toRewardRedemption(red))
}

// MemberPendingRedemptions lists one member's requests waiting for approval.
// The member's PIN travels in X-Member-PIN, or the admin's in X-Admin-PIN.
// GET /api/v1/households/{householdID}/family-members/{memberID}/reward-redemptions/pending
func (h *RewardHandler) MemberPendingRedemptions(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.ListMemberPending(r.Context(), currentUser(r), hid, mid, r.Header.Get("X-Member-PIN"), r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[rewardRedemptionJSON]{Items: mapSlice(rows, toRewardRedemption)})
}

// RewardHistory lists one member's approved redemptions, newest approval
// first. The member's PIN travels in X-Member-PIN, or the admin's in
// X-Admin-PIN. ?limit= and ?offset= page it by historyPage, the rule
// points-history uses.
// GET /api/v1/households/{householdID}/family-members/{memberID}/reward-history
func (h *RewardHandler) RewardHistory(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	limit, offset, err := historyPage(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	rows, err := h.svc.RewardHistory(r.Context(), currentUser(r), hid, mid, r.Header.Get("X-Member-PIN"), r.Header.Get("X-Admin-PIN"), limit, offset)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[rewardRedemptionJSON]{Items: mapSlice(rows, toRewardRedemption)})
}

// PendingRedemptions lists the household's requests waiting for approval,
// oldest first, each with the member's name. The admin PIN travels in
// X-Admin-PIN. None waiting answers {"items": []}, never null.
func (h *RewardHandler) PendingRedemptions(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	rows, err := h.svc.ListPending(r.Context(), currentUser(r), hid, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[pendingRedemptionJSON]{Items: mapSlice(rows, toPendingRedemption)})
}

func parseRedemptionIDs(w http.ResponseWriter, r *http.Request) (hid, rid uuid.UUID, ok bool) {
	if hid, ok = pathUUID(w, r, "householdID"); !ok {
		return
	}
	rid, ok = pathUUID(w, r, "redemptionID")
	return
}

// approveRedemptionResponse is the approved request and the member at the
// new points total.
type approveRedemptionResponse struct {
	Redemption rewardRedemptionJSON `json:"redemption"`
	Member     familyMemberJSON     `json:"member"`
}

// redemptionResponse wraps a rejected request.
type redemptionResponse struct {
	Redemption rewardRedemptionJSON `json:"redemption"`
}

// ApproveRedemption approves a pending request and takes its cost from the
// member's points. The admin PIN travels in X-Admin-PIN; the body is empty
// or {}. A member now short of points is a 422 on points and the request
// stays pending.
func (h *RewardHandler) ApproveRedemption(w http.ResponseWriter, r *http.Request) {
	hid, rid, ok := parseRedemptionIDs(w, r)
	if !ok || !decodeEmptyBody(w, r) {
		return
	}
	red, member, err := h.svc.Approve(r.Context(), currentUser(r), hid, rid, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, approveRedemptionResponse{Redemption: toRewardRedemption(red), Member: toFamilyMember(member)})
}

// RejectRedemption rejects a pending request; no points move. The admin PIN
// travels in X-Admin-PIN; the body is empty or {}.
func (h *RewardHandler) RejectRedemption(w http.ResponseWriter, r *http.Request) {
	hid, rid, ok := parseRedemptionIDs(w, r)
	if !ok || !decodeEmptyBody(w, r) {
		return
	}
	red, err := h.svc.Reject(r.Context(), currentUser(r), hid, rid, r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, redemptionResponse{Redemption: toRewardRedemption(red)})
}
