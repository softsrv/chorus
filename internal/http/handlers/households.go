package handlers

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

type householdServicer interface {
	List(ctx context.Context, userID uuid.UUID) ([]app.Household, error)
	Get(ctx context.Context, userID, householdID uuid.UUID) (app.Household, error)
	Create(ctx context.Context, userID uuid.UUID, in app.HouseholdInput) (app.Household, error)
	Update(ctx context.Context, userID, householdID uuid.UUID, in app.HouseholdInput) (app.Household, error)
	Delete(ctx context.Context, userID, householdID uuid.UUID) error
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
