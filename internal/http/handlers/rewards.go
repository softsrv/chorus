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
}

// RewardHandler serves /api/v1/households/{householdID}/rewards and
// /api/v1/reward-library. Writes carry the admin PIN in X-Admin-PIN. There is
// no redeem, buy or spend action.
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
