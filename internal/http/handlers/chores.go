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

const defaultPageSize = 50

type choreServicer interface {
	List(ctx context.Context, userID, householdID uuid.UUID, f app.ChoreFilter) ([]db.Chore, bool, error)
	Get(ctx context.Context, userID, householdID, choreID uuid.UUID) (db.Chore, error)
	Create(ctx context.Context, userID, householdID uuid.UUID, in app.ChoreInput) (db.Chore, error)
	Update(ctx context.Context, userID, householdID, choreID uuid.UUID, in app.ChoreInput) (db.Chore, error)
	SetCompleted(ctx context.Context, userID, householdID, choreID uuid.UUID, completed bool) (db.Chore, error)
	Delete(ctx context.Context, userID, householdID, choreID uuid.UUID) error
}

// ChoreHandler serves /api/v1/households/{householdID}/chores.
type ChoreHandler struct {
	svc choreServicer
}

// NewChoreHandler constructs a ChoreHandler.
func NewChoreHandler(svc choreServicer) *ChoreHandler {
	return &ChoreHandler{svc: svc}
}

// choreRequest is the body for create and update. Update is a full
// replacement (PUT): omitted optional fields are cleared.
type choreRequest struct {
	Title       string     `json:"title"`
	Description string     `json:"description"`
	AssigneeID  *uuid.UUID `json:"assignee_id"`
	DueOn       *string    `json:"due_on"` // YYYY-MM-DD
}

func (req choreRequest) toInput() (app.ChoreInput, error) {
	in := app.ChoreInput{Title: req.Title, Description: req.Description, AssigneeID: req.AssigneeID}
	if req.DueOn != nil {
		d, err := time.Parse(dateLayout, *req.DueOn)
		if err != nil {
			return in, &app.ValidationError{Fields: map[string]string{"due_on": "must be a date in YYYY-MM-DD format"}}
		}
		in.DueOn = &d
	}
	return in, nil
}

// choreListResponse adds a pagination cursor: next_offset is null on the
// last page.
type choreListResponse struct {
	Items      []choreJSON `json:"items"`
	NextOffset *int        `json:"next_offset"`
}

// List supports ?status=open|done|all (default open), ?assignee=<user id>|me,
// and ?limit= / ?offset= pagination.
func (h *ChoreHandler) List(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	userID := currentUser(r)
	q := r.URL.Query()

	f := app.ChoreFilter{Status: q.Get("status")}
	var err error
	if f.Limit, err = queryInt(r, "limit", defaultPageSize); err != nil {
		writeError(w, r, err)
		return
	}
	if f.Offset, err = queryInt(r, "offset", 0); err != nil {
		writeError(w, r, err)
		return
	}
	switch a := q.Get("assignee"); a {
	case "":
	case "me":
		f.AssigneeID = &userID
	default:
		id, parseErr := uuid.Parse(a)
		if parseErr != nil {
			respond.ValidationError(w, map[string]string{"assignee": "must be a user ID or \"me\""})
			return
		}
		f.AssigneeID = &id
	}

	chores, more, err := h.svc.List(r.Context(), userID, hid, f)
	if err != nil {
		writeError(w, r, err)
		return
	}
	resp := choreListResponse{Items: mapSlice(chores, toChore)}
	if more {
		next := f.Offset + len(chores)
		resp.NextOffset = &next
	}
	respond.JSON(w, http.StatusOK, resp)
}

func (h *ChoreHandler) Create(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req choreRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in, err := req.toInput()
	if err != nil {
		writeError(w, r, err)
		return
	}
	chore, err := h.svc.Create(r.Context(), currentUser(r), hid, in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toChore(chore))
}

func (h *ChoreHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := choreIDs(w, r)
	if !ok {
		return
	}
	chore, err := h.svc.Get(r.Context(), currentUser(r), hid, cid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toChore(chore))
}

func (h *ChoreHandler) Update(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := choreIDs(w, r)
	if !ok {
		return
	}
	var req choreRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	in, err := req.toInput()
	if err != nil {
		writeError(w, r, err)
		return
	}
	chore, err := h.svc.Update(r.Context(), currentUser(r), hid, cid, in)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toChore(chore))
}

// Complete marks a chore done (PUT) or reopens it (DELETE).
func (h *ChoreHandler) Complete(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := choreIDs(w, r)
	if !ok {
		return
	}
	chore, err := h.svc.SetCompleted(r.Context(), currentUser(r), hid, cid, r.Method == http.MethodPut)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toChore(chore))
}

func (h *ChoreHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid, cid, ok := choreIDs(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), currentUser(r), hid, cid); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

func choreIDs(w http.ResponseWriter, r *http.Request) (householdID, choreID uuid.UUID, ok bool) {
	if householdID, ok = pathUUID(w, r, "householdID"); !ok {
		return
	}
	choreID, ok = pathUUID(w, r, "choreID")
	return
}
