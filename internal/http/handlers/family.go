package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

type familyServicer interface {
	List(ctx context.Context, userID, householdID uuid.UUID) ([]app.Member, error)
	Get(ctx context.Context, userID, householdID, memberID uuid.UUID) (app.Member, error)
	Create(ctx context.Context, userID, householdID uuid.UUID, in app.FamilyMemberInput) (app.Member, error)
	Rename(ctx context.Context, userID, householdID, memberID uuid.UUID, in app.RenameMemberInput) (app.Member, error)
	Remove(ctx context.Context, userID, householdID, memberID uuid.UUID) error
	SetPin(ctx context.Context, userID, householdID, memberID uuid.UUID, pin string) (app.Member, error)
	ClearPin(ctx context.Context, userID, householdID, memberID uuid.UUID) (app.Member, error)
}

// FamilyMemberHandler serves family member CRUD endpoints.
type FamilyMemberHandler struct {
	svc familyServicer
}

// FamilyHandler is an alias for FamilyMemberHandler.
type FamilyHandler = FamilyMemberHandler

// NewFamilyMemberHandler constructs a FamilyMemberHandler.
func NewFamilyMemberHandler(svc familyServicer) *FamilyMemberHandler {
	return &FamilyMemberHandler{svc: svc}
}

// NewFamilyHandler constructs a FamilyHandler.
func NewFamilyHandler(svc familyServicer) *FamilyMemberHandler {
	return NewFamilyMemberHandler(svc)
}

type memberResponse struct {
	ID          uuid.UUID `json:"id"`
	HouseholdID uuid.UUID `json:"household_id"`
	Name        string    `json:"name"`
	HasPin      bool      `json:"has_pin"`
	Points      int32     `json:"points"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toMemberResponse(m app.Member) memberResponse {
	return memberResponse{
		ID:          m.ID,
		HouseholdID: m.HouseholdID,
		Name:        m.Name,
		HasPin:      m.HasPin,
		Points:      m.Points,
		CreatedAt:   m.CreatedAt.UTC(),
		UpdatedAt:   m.UpdatedAt.UTC(),
	}
}

type createMemberRequest struct {
	Name string `json:"name"`
	PIN  string `json:"pin"`
}

type renameMemberRequest struct {
	Name string `json:"name"`
}

type setMemberPinRequest struct {
	PIN string `json:"pin"`
}

func parseHouseholdID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	if r.PathValue("householdID") != "" {
		return pathUUID(w, r, "householdID")
	}
	return pathUUID(w, r, "id")
}

func parseMemberIDs(w http.ResponseWriter, r *http.Request) (hid, mid uuid.UUID, ok bool) {
	if hid, ok = parseHouseholdID(w, r); !ok {
		return
	}
	if r.PathValue("memberID") != "" {
		mid, ok = pathUUID(w, r, "memberID")
		return
	}
	mid, ok = pathUUID(w, r, "id")
	return
}

// List returns all family members in the household.
func (h *FamilyMemberHandler) List(w http.ResponseWriter, r *http.Request) {
	hid, ok := parseHouseholdID(w, r)
	if !ok {
		return
	}
	members, err := h.svc.List(r.Context(), currentUser(r), hid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[memberResponse]{
		Items: mapSlice(members, toMemberResponse),
	})
}

// Create adds a new family member to the household.
func (h *FamilyMemberHandler) Create(w http.ResponseWriter, r *http.Request) {
	hid, ok := parseHouseholdID(w, r)
	if !ok {
		return
	}
	var req createMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m, err := h.svc.Create(r.Context(), currentUser(r), hid, app.FamilyMemberInput{
		Name: req.Name,
		PIN:  req.PIN,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toMemberResponse(m))
}

// Get returns one family member.
func (h *FamilyMemberHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	m, err := h.svc.Get(r.Context(), currentUser(r), hid, mid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toMemberResponse(m))
}

// Rename updates a family member's name.
func (h *FamilyMemberHandler) Rename(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	var req renameMemberRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m, err := h.svc.Rename(r.Context(), currentUser(r), hid, mid, app.RenameMemberInput{
		Name: req.Name,
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toMemberResponse(m))
}

// Delete removes a family member.
func (h *FamilyMemberHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	if err := h.svc.Remove(r.Context(), currentUser(r), hid, mid); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

// SetPin sets or updates a family member's PIN.
func (h *FamilyMemberHandler) SetPin(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	var req setMemberPinRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m, err := h.svc.SetPin(r.Context(), currentUser(r), hid, mid, req.PIN)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toMemberResponse(m))
}

// ClearPin removes a family member's PIN.
func (h *FamilyMemberHandler) ClearPin(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	m, err := h.svc.ClearPin(r.Context(), currentUser(r), hid, mid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toMemberResponse(m))
}
