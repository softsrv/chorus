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
	Create(ctx context.Context, userID, householdID uuid.UUID, in app.FamilyMemberInput, adminPin string) (app.Member, error)
	Rename(ctx context.Context, userID, householdID, memberID uuid.UUID, in app.RenameMemberInput, adminPin string) (app.Member, error)
	Remove(ctx context.Context, userID, householdID, memberID uuid.UUID, adminPin string) error
	SetPin(ctx context.Context, userID, householdID, memberID uuid.UUID, pin string, adminPin string) (app.Member, error)
	ClearPin(ctx context.Context, userID, householdID, memberID uuid.UUID, adminPin string) (app.Member, error)
	Verify(ctx context.Context, userID, householdID, memberID uuid.UUID, pin string) (app.Member, error)
	PointsHistory(ctx context.Context, userID, householdID, memberID uuid.UUID, memberPin, adminPin string, limit, offset int32) ([]app.PointsAward, error)
	AdminPinSet(ctx context.Context, userID, householdID uuid.UUID) (bool, error)
	SetAdminPin(ctx context.Context, userID, householdID uuid.UUID, newPin, currentAdminPin string) error
	ClearAdminPin(ctx context.Context, userID, householdID uuid.UUID, currentAdminPin string) error
	VerifyAdminPin(ctx context.Context, userID, householdID uuid.UUID, pin string) error
}

// FamilyMemberHandler serves family member CRUD endpoints.
type FamilyMemberHandler struct {
	svc familyServicer
}

// NewFamilyMemberHandler constructs a FamilyMemberHandler.
func NewFamilyMemberHandler(svc familyServicer) *FamilyMemberHandler {
	return &FamilyMemberHandler{svc: svc}
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

type verifyMemberPinRequest struct {
	PIN string `json:"pin"`
}

type adminPinRequest struct {
	PIN string `json:"pin"`
}

type adminPinStatusResponse struct {
	Set bool `json:"set"`
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
	items := mapSlice(members, toMemberResponse)
	if items == nil {
		items = []memberResponse{}
	}
	respond.JSON(w, http.StatusOK, listResponse[memberResponse]{
		Items: items,
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
	adminPin := r.Header.Get("X-Admin-PIN")
	m, err := h.svc.Create(r.Context(), currentUser(r), hid, app.FamilyMemberInput{
		Name: req.Name,
		PIN:  req.PIN,
	}, adminPin)
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
	adminPin := r.Header.Get("X-Admin-PIN")
	m, err := h.svc.Rename(r.Context(), currentUser(r), hid, mid, app.RenameMemberInput{
		Name: req.Name,
	}, adminPin)
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
	adminPin := r.Header.Get("X-Admin-PIN")
	if err := h.svc.Remove(r.Context(), currentUser(r), hid, mid, adminPin); err != nil {
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
	adminPin := r.Header.Get("X-Admin-PIN")
	m, err := h.svc.SetPin(r.Context(), currentUser(r), hid, mid, req.PIN, adminPin)
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
	adminPin := r.Header.Get("X-Admin-PIN")
	m, err := h.svc.ClearPin(r.Context(), currentUser(r), hid, mid, adminPin)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toMemberResponse(m))
}

// Verify verifies a family member's PIN.
// POST /api/v1/households/{householdID}/family-members/{memberID}/verify
func (h *FamilyMemberHandler) Verify(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	var req verifyMemberPinRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	m, err := h.svc.Verify(r.Context(), currentUser(r), hid, mid, req.PIN)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toMemberResponse(m))
}

// AdminPinStatus returns whether an admin PIN is set.
// GET /api/v1/households/{householdID}/admin-pin
func (h *FamilyMemberHandler) AdminPinStatus(w http.ResponseWriter, r *http.Request) {
	hid, ok := parseHouseholdID(w, r)
	if !ok {
		return
	}
	set, err := h.svc.AdminPinSet(r.Context(), currentUser(r), hid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, adminPinStatusResponse{Set: set})
}

// SetAdminPin sets or changes the household's admin PIN.
// PUT /api/v1/households/{householdID}/admin-pin
func (h *FamilyMemberHandler) SetAdminPin(w http.ResponseWriter, r *http.Request) {
	hid, ok := parseHouseholdID(w, r)
	if !ok {
		return
	}
	var req adminPinRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	currentAdminPin := r.Header.Get("X-Admin-PIN")
	if err := h.svc.SetAdminPin(r.Context(), currentUser(r), hid, req.PIN, currentAdminPin); err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, adminPinStatusResponse{Set: true})
}

// ClearAdminPin deletes the household's admin PIN.
// DELETE /api/v1/households/{householdID}/admin-pin
func (h *FamilyMemberHandler) ClearAdminPin(w http.ResponseWriter, r *http.Request) {
	hid, ok := parseHouseholdID(w, r)
	if !ok {
		return
	}
	currentAdminPin := r.Header.Get("X-Admin-PIN")
	if err := h.svc.ClearAdminPin(r.Context(), currentUser(r), hid, currentAdminPin); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

// VerifyAdminPin verifies the household's admin PIN.
// POST /api/v1/households/{householdID}/admin-pin/verify
func (h *FamilyMemberHandler) VerifyAdminPin(w http.ResponseWriter, r *http.Request) {
	hid, ok := parseHouseholdID(w, r)
	if !ok {
		return
	}
	var req adminPinRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.svc.VerifyAdminPin(r.Context(), currentUser(r), hid, req.PIN); err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, map[string]any{})
}

// PointsHistory returns a member's points awards newest first.
// GET /api/v1/households/{householdID}/family-members/{memberID}/points-history
func (h *FamilyMemberHandler) PointsHistory(w http.ResponseWriter, r *http.Request) {
	hid, mid, ok := parseMemberIDs(w, r)
	if !ok {
		return
	}
	limit, err := queryInt(r, "limit", defaultPageSize)
	if err != nil {
		writeError(w, r, err)
		return
	}
	offset, err := queryInt(r, "offset", 0)
	if err != nil {
		writeError(w, r, err)
		return
	}
	memberPin := r.Header.Get("X-Member-PIN")
	adminPin := r.Header.Get("X-Admin-PIN")
	awards, err := h.svc.PointsHistory(r.Context(), currentUser(r), hid, mid, memberPin, adminPin, int32(limit), int32(offset))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[pointsAwardJSON]{
		Items: mapSlice(awards, toPointsAward),
	})
}

