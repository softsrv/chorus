package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/http/respond"
)

// familyCalendarServicer is the subset of app.FamilyCalendar the handler needs.
type familyCalendarServicer interface {
	ListMyCalendars(ctx context.Context, userID uuid.UUID) (app.MyCalendars, error)
	Set(ctx context.Context, userID, householdID uuid.UUID, calendarID string) (app.HouseholdCalendarStatus, error)
	Clear(ctx context.Context, userID, householdID uuid.UUID) error
	Status(ctx context.Context, userID, householdID uuid.UUID) (app.HouseholdCalendarStatus, error)
	ListEntries(ctx context.Context, userID, householdID uuid.UUID, from, to time.Time) (app.HouseholdEntries, error)
}

// FamilyCalendarHandler serves the caller's Google calendar list and each
// household's family calendar and its entries. The user always comes from the
// auth context; household responses never carry a token or a calendar list.
type FamilyCalendarHandler struct {
	svc familyCalendarServicer
}

// NewFamilyCalendarHandler constructs a FamilyCalendarHandler.
func NewFamilyCalendarHandler(svc familyCalendarServicer) *FamilyCalendarHandler {
	return &FamilyCalendarHandler{svc: svc}
}

type googleCalendarJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Primary bool   `json:"primary"`
}

type myCalendarsResponse struct {
	Status string               `json:"status"`
	Items  []googleCalendarJSON `json:"items"`
}

type setByJSON struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"display_name"`
}

type householdCalendarJSON struct {
	Status       string     `json:"status"`
	CalendarID   string     `json:"calendar_id,omitempty"`
	CalendarName string     `json:"calendar_name,omitempty"`
	SetBy        *setByJSON `json:"set_by,omitempty"`
}

type familyEntryJSON struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Start    string `json:"start"`
	End      string `json:"end"`
	AllDay   bool   `json:"all_day"`
	Location string `json:"location"`
}

type householdEntriesResponse struct {
	Status string            `json:"status"`
	Items  []familyEntryJSON `json:"items"`
}

type setCalendarRequest struct {
	CalendarID string `json:"calendar_id"`
}

func toHouseholdCalendar(st app.HouseholdCalendarStatus) householdCalendarJSON {
	out := householdCalendarJSON{Status: st.Status}
	if st.CalendarID != "" {
		out.CalendarID = st.CalendarID
		out.CalendarName = st.CalendarName
		out.SetBy = &setByJSON{ID: st.SetByID, DisplayName: st.SetByName}
	}
	return out
}

// MyCalendars lists the caller's own Google calendars, to pick one from.
func (h *FamilyCalendarHandler) MyCalendars(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.ListMyCalendars(r.Context(), currentUser(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, myCalendarsResponse{
		Status: res.Status,
		Items: mapSlice(res.Calendars, func(c app.GoogleCalendarInfo) googleCalendarJSON {
			return googleCalendarJSON{ID: c.ID, Name: c.Name, Primary: c.Primary}
		}),
	})
}

// Status reports the household's family calendar.
func (h *FamilyCalendarHandler) Status(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	st, err := h.svc.Status(r.Context(), currentUser(r), hid)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toHouseholdCalendar(st))
}

// Set makes one of the caller's calendars the household's family calendar.
func (h *FamilyCalendarHandler) Set(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	var req setCalendarRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	st, err := h.svc.Set(r.Context(), currentUser(r), hid, req.CalendarID)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toHouseholdCalendar(st))
}

// Clear removes the household's family calendar (setter only).
func (h *FamilyCalendarHandler) Clear(w http.ResponseWriter, r *http.Request) {
	hid, ok := pathUUID(w, r, "householdID")
	if !ok {
		return
	}
	if err := h.svc.Clear(r.Context(), currentUser(r), hid); err != nil {
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

// Events requires ?from= and ?to= (RFC 3339, at most 100 days apart) and
// returns the family calendar's entries overlapping that range with a status.
func (h *FamilyCalendarHandler) Events(w http.ResponseWriter, r *http.Request) {
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
	res, err := h.svc.ListEntries(r.Context(), currentUser(r), hid, from, to)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, householdEntriesResponse{
		Status: res.Status,
		Items: mapSlice(res.Entries, func(e app.GoogleEntry) familyEntryJSON {
			return familyEntryJSON{ID: e.ID, Title: e.Title, Start: e.Start, End: e.End, AllDay: e.AllDay, Location: e.Location}
		}),
	})
}
