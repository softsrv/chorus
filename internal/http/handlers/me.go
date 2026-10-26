package handlers

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/softsrv/chorus/internal/app"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/http/middleware"
	"github.com/softsrv/chorus/internal/http/respond"
)

type accountAuthServicer interface {
	ChangePassword(ctx context.Context, userID, currentSessionID uuid.UUID, currentPassword, newPassword string) error
	DeleteAccount(ctx context.Context, userID, currentSessionID uuid.UUID, password, confirmEmail string) error
	ResendVerification(ctx context.Context, userID uuid.UUID) error
}

type userServicer interface {
	UpdateProfile(ctx context.Context, userID uuid.UUID, displayName string) (db.User, error)
	ListSessions(ctx context.Context, userID uuid.UUID) ([]db.RefreshToken, error)
	RevokeSession(ctx context.Context, userID, tokenID uuid.UUID) (db.RefreshToken, error)
}

// MeHandler serves /api/v1/me: the signed-in user's own account.
type MeHandler struct {
	auth  accountAuthServicer
	users userServicer
}

// NewMeHandler constructs a MeHandler.
func NewMeHandler(authSvc accountAuthServicer, userSvc userServicer) *MeHandler {
	return &MeHandler{auth: authSvc, users: userSvc}
}

func (h *MeHandler) Get(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.UserFromContext(r.Context())
	respond.JSON(w, http.StatusOK, toUser(user))
}

func (h *MeHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	user, err := h.users.UpdateProfile(r.Context(), currentUser(r), req.DisplayName)
	if err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusOK, toUser(user))
}

func (h *MeHandler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sessionID, _ := middleware.SessionIDFromContext(r.Context())
	if err := h.auth.ChangePassword(r.Context(), currentUser(r), sessionID, req.CurrentPassword, req.NewPassword); err != nil {
		if errors.Is(err, app.ErrInvalidCredentials) {
			respond.ValidationError(w, map[string]string{"current_password": "is incorrect"})
			return
		}
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

// Delete permanently deletes the account. The password is required again so a
// stolen access token alone can't destroy the account. A passwordless
// (Google-only) user types their account email instead, from a session they
// signed in to within the last few minutes.
func (h *MeHandler) Delete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
		Email    string `json:"email"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	sessionID, _ := middleware.SessionIDFromContext(r.Context())
	if err := h.auth.DeleteAccount(r.Context(), currentUser(r), sessionID, req.Password, req.Email); err != nil {
		if errors.Is(err, app.ErrInvalidCredentials) {
			respond.ValidationError(w, map[string]string{"password": "is incorrect"})
			return
		}
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}

func (h *MeHandler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	if err := h.auth.ResendVerification(r.Context(), currentUser(r)); err != nil {
		writeError(w, r, err)
		return
	}
	respond.JSON(w, http.StatusAccepted, map[string]string{"message": "Verification email sent."})
}

func (h *MeHandler) ListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.users.ListSessions(r.Context(), currentUser(r))
	if err != nil {
		writeError(w, r, err)
		return
	}
	current, _ := middleware.SessionIDFromContext(r.Context())
	respond.JSON(w, http.StatusOK, listResponse[sessionJSON]{
		Items: mapSlice(sessions, func(rt db.RefreshToken) sessionJSON { return toSession(rt, current) }),
	})
}

// RevokeSession signs out one of the user's devices. Revoking the current
// session is allowed and behaves like logout.
func (h *MeHandler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if _, err := h.users.RevokeSession(r.Context(), currentUser(r), id); err != nil {
		if errors.Is(err, app.ErrForbidden) || errors.Is(err, app.ErrTokenNotFound) {
			respond.Error(w, http.StatusNotFound, "not_found", "Not found.")
			return
		}
		writeError(w, r, err)
		return
	}
	respond.NoContent(w)
}
