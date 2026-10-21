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

type eventServicer interface {
	ListRange(ctx context.Context, userID, householdID uuid.UUID, from, to time.Time) ([]db.Event, error)
	Get(ctx context.Context, userID, householdID, eventID uuid.UUID) (db.Event, error)
	Create(ctx context.Context, userID, householdID uuid.UUID, in app.EventInput) (db.Event, error)
	Update(ctx context.Context, userID, householdID, eventID uuid.UUID, in app.EventInput) (db.Event, error)
	Delete(ctx context.Context, userID, householdID, eventID uuid.UUID) error
}

// EventHandler serves /api/v1/households/{householdID}/events.
type EventHandler struct {
	svc eventServicer
}

// NewEventHandler constructs an EventHandler.
func NewEventHandler(svc eventServicer) *EventHandler {
	return &EventHandler{svc: svc}
}

// eventRequest is the body for create and update (full replacement).
type eventRequest struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Location    string    `json:"location"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	AllDay      bool      `json:"all_day"`
}

// List requires ?from= and ?to= (RFC 3339) and returns events overlapping
// that range, which may span at most 100 days.
func (h *EventHandler) List(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	from, err := queryTime(r, "from")
	if err != nil {
		writeError(w, r, err)
		return
	}
	to, err := queryTime(r, "to")
	if err != nil {
		writeError(w, r, err)
		return
	}
	events, err := h.svc.ListRange(r.Context(), currentUser(r), hid, from, to)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, listResponse[eventJSON]{Items: mapSlice(events, toEvent)})
}

func (h *EventHandler) Create(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req eventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	event, err := h.svc.Create(r.Context(), currentUser(r), hid, app.EventInput(req))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusCreated, toEvent(event))
}

func (h *EventHandler) Get(w http.ResponseWriter, r *http.Request) {
	hid, eid, ok := eventIDs(w, r)
	if !ok {
		return
	}
	event, err := h.svc.Get(r.Context(), currentUser(r), hid, eid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toEvent(event))
}

func (h *EventHandler) Update(w http.ResponseWriter, r *http.Request) {
	hid, eid, ok := eventIDs(w, r)
	if !ok {
		return
	}
	var req eventRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	event, err := h.svc.Update(r.Context(), currentUser(r), hid, eid, app.EventInput(req))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toEvent(event))
}

func (h *EventHandler) Delete(w http.ResponseWriter, r *http.Request) {
	hid, eid, ok := eventIDs(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), currentUser(r), hid, eid); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

func eventIDs(w http.ResponseWriter, r *http.Request) (householdID, eventID uuid.UUID, ok bool) {
	if householdID, ok = pathUUID(w, r, "householdID"); !ok {
		return
	}
	eventID, ok = pathUUID(w, r, "eventID")
	return
}
