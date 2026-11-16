package handlers

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

type comboServicer interface {
	List(ctx context.Context, userID, householdID uuid.UUID) ([]app.Combo, error)
	Get(ctx context.Context, userID, householdID, comboID uuid.UUID) (app.Combo, error)
	Create(ctx context.Context, userID, householdID uuid.UUID, in app.ComboInput, adminPin string) (app.Combo, error)
	Update(ctx context.Context, userID, householdID, comboID uuid.UUID, in app.ComboInput, adminPin string) (app.Combo, error)
	Delete(ctx context.Context, userID, householdID, comboID uuid.UUID, adminPin string) error
}

// ComboHandler serves /api/v1/households/{householdID}/combos.
// Writes carry the admin PIN in X-Admin-PIN.
type ComboHandler struct {
	svc comboServicer
}

// NewComboHandler constructs a ComboHandler.
func NewComboHandler(svc comboServicer) *ComboHandler {
	return &ComboHandler{svc: svc}
}

type comboRequest struct {
	Name        string          `json:"name"`
	BonusPoints json.RawMessage `json:"bonus_points"`
	QuestIDs    []uuid.UUID     `json:"quest_ids"`
}

func (req comboRequest) toInput() app.ComboInput {
	return app.ComboInput{
		Name:        req.Name,
		BonusPoints: numberField(req.BonusPoints),
		QuestIDs:    req.QuestIDs,
	}
}

func parseComboIDs(w http.ResponseWriter, r *http.Request) (hid, cid uuid.UUID, ok bool) {
	if hid, ok = pathUUID(w, r, "householdID"); !ok {
		return
	}
	cid, ok = pathUUID(w, r, "comboID")
	return
}

// List returns every combo of the household. An empty household answers
// {"items": []}, never null.
func (h *ComboHandler) List(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	combos, err := h.svc.List(r.Context(), currentUser(r), hid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[comboJSON]{Items: mapSlice(combos, toCombo)})
}

// Get returns one combo.
func (h *ComboHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := parseComboIDs(w, r)
	if !ok {
		return
	}
	combo, err := h.svc.Get(r.Context(), currentUser(r), hid, cid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toCombo(combo))
}

// Create adds a combo: {"name": "...", "bonus_points": 20, "quest_ids": [...]}.
func (h *ComboHandler) Create(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req comboRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	combo, err := h.svc.Create(r.Context(), currentUser(r), hid, req.toInput(), r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toCombo(combo))
}

// Update replaces a combo's name, bonus_points, and quest_ids (PUT).
func (h *ComboHandler) Update(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := parseComboIDs(w, r)
	if !ok {
		return
	}
	var req comboRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	combo, err := h.svc.Update(r.Context(), currentUser(r), hid, cid, req.toInput(), r.Header.Get("X-Admin-PIN"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toCombo(combo))
}

// Delete removes a combo.
func (h *ComboHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := parseComboIDs(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), currentUser(r), hid, cid, r.Header.Get("X-Admin-PIN")); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}
