package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/respond"
)

type householdServicer interface {
	List(ctx context.Context, userID uuid.UUID) ([]app.Household, error)
	Get(ctx context.Context, userID, householdID uuid.UUID) (app.Household, error)
	Create(ctx context.Context, userID uuid.UUID, in app.HouseholdInput) (app.Household, error)
	Update(ctx context.Context, userID, householdID uuid.UUID, in app.HouseholdInput) (app.Household, error)
	Delete(ctx context.Context, userID, householdID uuid.UUID) error
	Members(ctx context.Context, userID, householdID uuid.UUID) ([]db.ListHouseholdMembersRow, error)
	SetMemberRole(ctx context.Context, userID, householdID, memberID uuid.UUID, role string) error
	RemoveMember(ctx context.Context, userID, householdID, memberID uuid.UUID) error
	CreateInvite(ctx context.Context, userID, householdID uuid.UUID) (app.Invite, error)
	AcceptInvite(ctx context.Context, userID uuid.UUID, code string) (app.Household, error)
}

// HouseholdHandler serves households, membership, and invites.
type HouseholdHandler struct {
	svc householdServicer
}

// NewHouseholdHandler constructs a HouseholdHandler.
func NewHouseholdHandler(svc householdServicer) *HouseholdHandler {
	return &HouseholdHandler{svc: svc}
}

type householdRequest struct {
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
}

func (h *HouseholdHandler) List(w http.ResponseWriter, r *http.Request) {
	households, err := h.svc.List(r.Context(), currentUser(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[householdJSON]{Items: mapSlice(households, toHousehold)})
}

func (h *HouseholdHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req householdRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	household, err := h.svc.Create(r.Context(), currentUser(r), app.HouseholdInput(req))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toHousehold(household))
}

func (h *HouseholdHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	household, err := h.svc.Get(r.Context(), currentUser(r), hid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toHousehold(household))
}

func (h *HouseholdHandler) Update(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req householdRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	household, err := h.svc.Update(r.Context(), currentUser(r), hid, app.HouseholdInput(req))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toHousehold(household))
}

func (h *HouseholdHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), currentUser(r), hid); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

func (h *HouseholdHandler) ListMembers(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	members, err := h.svc.Members(r.Context(), currentUser(r), hid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[memberJSON]{Items: mapSlice(members, toMember)})
}

func (h *HouseholdHandler) UpdateMember(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	uid, ok := pathUUID(w, r, "userID")
	if !ok {
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.svc.SetMemberRole(r.Context(), currentUser(r), hid, uid, req.Role); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

// RemoveMember removes a member. Use your own user ID to leave a household.
func (h *HouseholdHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	uid, ok := pathUUID(w, r, "userID")
	if !ok {
		return
	}
	if err := h.svc.RemoveMember(r.Context(), currentUser(r), hid, uid); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

func (h *HouseholdHandler) CreateInvite(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	inv, err := h.svc.CreateInvite(r.Context(), currentUser(r), hid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, struct {
		Code      string    `json:"code"`
		ExpiresAt time.Time `json:"expires_at"`
	}{inv.Code, inv.ExpiresAt.UTC()})
}

func (h *HouseholdHandler) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	household, err := h.svc.AcceptInvite(r.Context(), currentUser(r), req.Code)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toHousehold(household))
}
