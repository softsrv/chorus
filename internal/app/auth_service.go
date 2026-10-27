package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"

	"github.com/softsrv/chorus/internal/auth"
	"github.com/softsrv/chorus/internal/db"
	"github.com/softsrv/chorus/internal/email"
	"github.com/softsrv/chorus/internal/users"
)

const (
	maxFailedLoginAttempts = 10
	lockDuration           = time.Hour
	// recentSignInWindow is how fresh a passwordless user's session must be
	// for destructive account actions that would otherwise need a password.
	recentSignInWindow = 10 * time.Minute
)

// hasPassword reports whether the account can sign in with a password.
// Google-only accounts have a NULL (or empty) password_hash.
func hasPassword(u db.User) bool {
	return u.PasswordHash.Valid && u.PasswordHash.String != ""
}

// DeviceMeta holds metadata captured from the HTTP request.
type DeviceMeta struct {
	DeviceName string
	IPAddress  *netip.Addr
	UserAgent  string
}

// TokenResult is returned after a successful login or token refresh.
type TokenResult struct {
	AccessToken        string
	AccessTokenExpiry  time.Time
	RefreshToken       string
	RefreshTokenExpiry time.Time
	User               db.User
}

// pgxBeginner is satisfied by *pgxpool.Pool.
type pgxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// AuthServiceConfig holds the configuration for AuthService.
// Using a config struct instead of positional parameters makes call sites
// self-documenting and prevents accidental argument transposition.
type AuthServiceConfig struct {
	JWTSecret      string
	AccessExpiry   time.Duration
	RefreshExpiry  time.Duration
	BCryptCost     int
	PasswordMinLen int
	AppBaseURL     string
	AppName        string
}

// AuthService handles all authentication business logic.
type AuthService struct {
	q      *db.Queries
	pool   pgxBeginner
	mailer email.Mailer
	cfg    AuthServiceConfig

	// dummyHash is compared against the supplied password when the email does
	// not exist, so the login path spends comparable time whether or not the
	// account is real. This closes the timing oracle that would otherwise leak
	// which emails are registered.
	dummyHash []byte

	// wg tracks background email goroutines so Shutdown can drain them cleanly.
	wg sync.WaitGroup
}

// NewAuthService constructs an AuthService with all dependencies injected.
func NewAuthService(q *db.Queries, pool pgxBeginner, mailer email.Mailer, cfg AuthServiceConfig) *AuthService {
	// Precompute a throwaway hash at the configured cost so the no-user login
	// path can perform an equivalent bcrypt comparison. If generation fails we
	// leave it nil; Login handles that by skipping the comparison.
	dummyHash, err := bcrypt.GenerateFromPassword([]byte("timing-equalizer-not-a-real-password"), cfg.BCryptCost)
	if err != nil {
		slog.Error("generate dummy password hash", "error", err)
	}

	return &AuthService{
		q:         q,
		pool:      pool,
		mailer:    mailer,
		cfg:       cfg,
		dummyHash: dummyHash,
	}
}

// Shutdown blocks until all in-flight background email goroutines have finished.
// Call this during application shutdown, after the HTTP server has stopped
// accepting new requests, to avoid cutting off in-progress email deliveries.
func (s *AuthService) Shutdown() {
	s.wg.Wait()
}

// goSend runs fn in a tracked background goroutine. The WaitGroup is incremented
// before launching so that Shutdown() can drain all outstanding sends.
func (s *AuthService) goSend(fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// Register creates a new user account and sends an email verification link.
//
// If the email is already registered, Register still returns nil and instead
// emails the existing owner. Callers therefore get an identical response either
// way, so registration can't be used to discover which emails have accounts.
func (s *AuthService) Register(ctx context.Context, rawEmail, password, displayName string) error {
	var v validator
	normalizedEmail := users.NormalizeEmail(rawEmail)
	if err := users.ValidateEmail(normalizedEmail); err != nil {
		v.fail("email", "is not a valid email address")
	}
	if err := users.ValidatePassword(password, s.cfg.PasswordMinLen); err != nil {
		v.fail("password", passwordProblem(err, s.cfg.PasswordMinLen))
	}
	displayName = v.displayName(displayName)
	if err := v.err(); err != nil {
		return err
	}

	// Hash before touching the database so the duplicate-email path costs the
	// same bcrypt time as the success path.
	hash, err := bcrypt.GenerateFromPassword([]byte(password), s.cfg.BCryptCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate user id: %w", err)
	}

	user, err := s.q.CreateUser(ctx, db.CreateUserParams{
		ID:           id,
		Email:        normalizedEmail,
		DisplayName:  displayName,
		PasswordHash: pgtype.Text{String: string(hash), Valid: true},
	})
	if err != nil {
		if isUniqueViolation(err) {
			subj, html, text := email.AccountExistsEmail(s.cfg.AppName, s.cfg.AppBaseURL+"/forgot-password")
			s.goSend(func() {
				if sendErr := s.mailer.Send(normalizedEmail, subj, html, text); sendErr != nil {
					slog.Error("send account exists email", "error", sendErr)
				}
			})
			return nil
		}
		return fmt.Errorf("create user: %w", err)
	}

	s.goSend(func() {
		if sendErr := s.sendVerificationEmail(context.Background(), user); sendErr != nil {
			slog.Error("send verification email", "error", sendErr, "user_id", user.ID)
		}
	})

	return nil
}

// Login authenticates a user and returns a token pair.
func (s *AuthService) Login(ctx context.Context, rawEmail, password string, meta DeviceMeta) (TokenResult, error) {
	normalizedEmail := users.NormalizeEmail(rawEmail)

	user, err := s.q.GetUserByEmail(ctx, normalizedEmail)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend comparable time to the real-password path so response
			// latency doesn't reveal whether the email is registered.
			if s.dummyHash != nil {
				_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
			}
			return TokenResult{}, ErrInvalidCredentials
		}
		return TokenResult{}, fmt.Errorf("get user: %w", err)
	}

	// A passwordless (Google-only) account can never sign in with a password.
	// Spend the same bcrypt time as the other paths, and refuse before the
	// lock check and the failed-attempts counter: guessing passwords against
	// such an account must neither count nor lock it.
	if !hasPassword(user) {
		if s.dummyHash != nil {
			_ = bcrypt.CompareHashAndPassword(s.dummyHash, []byte(password))
		}
		return TokenResult{}, ErrInvalidCredentials
	}

	// Check account lock before anything else.
	if user.LockedUntil.Valid && user.LockedUntil.Time.After(time.Now()) {
		return TokenResult{}, ErrAccountLocked
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(password)); err != nil {
		if incrErr := s.q.IncrementFailedLoginAttempts(ctx, user.ID); incrErr != nil {
			slog.Error("increment failed login attempts", "user_id", user.ID, "error", incrErr)
		}
		if user.FailedLoginAttempts+1 >= maxFailedLoginAttempts {
			lockedUntil := time.Now().Add(lockDuration)
			if lockErr := s.q.LockAccount(ctx, db.LockAccountParams{
				ID:          user.ID,
				LockedUntil: pgtype.Timestamptz{Time: lockedUntil, Valid: true},
			}); lockErr != nil {
				slog.Error("lock account", "user_id", user.ID, "error", lockErr)
			}
		}
		return TokenResult{}, ErrInvalidCredentials
	}

	if err := s.q.ResetLoginAttempts(ctx, user.ID); err != nil {
		slog.Error("reset login attempts", "error", err, "user_id", user.ID)
	}

	return s.issueTokenPair(ctx, user, meta)
}

// Logout revokes the session the caller's access token was issued under.
func (s *AuthService) Logout(ctx context.Context, sessionID uuid.UUID) error {
	if err := s.q.RevokeRefreshToken(ctx, sessionID); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// Refresh validates a raw refresh token, issues a new access token, and returns
// the same refresh token (no rotation). The refresh token's last_used_at and
// device metadata are updated in place.
func (s *AuthService) Refresh(ctx context.Context, rawRefreshToken string, meta DeviceMeta) (TokenResult, error) {
	hash := auth.HashToken(rawRefreshToken)

	rt, err := s.q.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenResult{}, ErrTokenNotFound
		}
		return TokenResult{}, fmt.Errorf("get refresh token: %w", err)
	}

	if rt.RevokedAt.Valid {
		return TokenResult{}, ErrTokenRevoked
	}

	if rt.ExpiresAt.Time.Before(time.Now()) {
		return TokenResult{}, ErrTokenExpired
	}

	user, err := s.q.GetUserByID(ctx, rt.UserID)
	if err != nil {
		return TokenResult{}, fmt.Errorf("get user: %w", err)
	}

	if err := s.q.UpdateRefreshTokenLastUsed(ctx, db.UpdateRefreshTokenLastUsedParams{
		ID:         rt.ID,
		DeviceName: pgtype.Text{String: meta.DeviceName, Valid: meta.DeviceName != ""},
		IpAddress:  meta.IPAddress,
		UserAgent:  pgtype.Text{String: meta.UserAgent, Valid: meta.UserAgent != ""},
	}); err != nil {
		slog.Error("update refresh token last used", "token_id", rt.ID, "error", err)
	}

	tp, err := auth.IssueAccessToken(user.ID, rt.ID, s.cfg.JWTSecret, s.cfg.AccessExpiry)
	if err != nil {
		return TokenResult{}, fmt.Errorf("issue access token: %w", err)
	}

	return TokenResult{
		AccessToken:        tp.AccessToken,
		AccessTokenExpiry:  tp.ExpiresAt,
		RefreshToken:       rawRefreshToken,
		RefreshTokenExpiry: rt.ExpiresAt.Time,
		User:               user,
	}, nil
}

// RequestPasswordReset sends a reset email if the address exists.
// Always returns nil to prevent email enumeration.
func (s *AuthService) RequestPasswordReset(ctx context.Context, rawEmail string) error {
	normalizedEmail := users.NormalizeEmail(rawEmail)

	user, err := s.q.GetUserByEmail(ctx, normalizedEmail)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("get user: %w", err)
	}

	count, err := s.q.CountRecentPasswordResetsByEmail(ctx, normalizedEmail)
	if err != nil {
		return fmt.Errorf("count resets: %w", err)
	}
	if count >= 3 {
		return nil
	}

	rawToken, hashedToken, err := auth.GenerateResetToken()
	if err != nil {
		return fmt.Errorf("generate reset token: %w", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate token id: %w", err)
	}
	_, err = s.q.InsertPasswordResetToken(ctx, db.InsertPasswordResetTokenParams{
		ID:        id,
		UserID:    user.ID,
		TokenHash: hashedToken,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("insert reset token: %w", err)
	}

	resetURL := fmt.Sprintf("%s/reset-password?token=%s", s.cfg.AppBaseURL, rawToken)
	subj, html, text := email.PasswordResetEmail(s.cfg.AppName, resetURL)
	s.goSend(func() {
		if sendErr := s.mailer.Send(normalizedEmail, subj, html, text); sendErr != nil {
			slog.Error("send reset email", "error", sendErr, "user_id", user.ID)
		}
	})

	return nil
}

// CompletePasswordReset validates the token and updates the user's password.
// All of the user's sessions are revoked.
func (s *AuthService) CompletePasswordReset(ctx context.Context, rawToken, newPassword string) error {
	if err := users.ValidatePassword(newPassword, s.cfg.PasswordMinLen); err != nil {
		return &ValidationError{Fields: map[string]string{"password": passwordProblem(err, s.cfg.PasswordMinLen)}}
	}

	hashedToken := auth.HashToken(rawToken)
	prt, err := s.q.GetPasswordResetTokenByHash(ctx, hashedToken)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTokenNotFound
		}
		return fmt.Errorf("get reset token: %w", err)
	}
	if prt.UsedAt.Valid {
		return ErrTokenUsed
	}
	if prt.ExpiresAt.Time.Before(time.Now()) {
		return ErrTokenExpired
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.cfg.BCryptCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	// Claim the token first; the conditional update means a concurrent
	// redemption of the same token finds zero rows and aborts.
	n, err := qtx.MarkPasswordResetTokenUsed(ctx, prt.ID)
	if err != nil {
		return fmt.Errorf("mark token used: %w", err)
	}
	if n == 0 {
		return ErrTokenUsed
	}
	if err := qtx.UpdatePasswordHash(ctx, db.UpdatePasswordHashParams{
		ID:           prt.UserID,
		PasswordHash: pgtype.Text{String: string(newHash), Valid: true},
	}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if err := qtx.RevokeAllUserRefreshTokens(ctx, prt.UserID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.notifyPasswordChanged(ctx, prt.UserID)
	return nil
}

// VerifyEmail validates a raw verification token from a link, marks the user's
// email as verified, and issues a token pair so the click auto-logs them in.
func (s *AuthService) VerifyEmail(ctx context.Context, rawToken string, meta DeviceMeta) (TokenResult, error) {
	hash := auth.HashToken(rawToken)
	record, err := s.q.GetVerificationCodeByTokenHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return TokenResult{}, ErrTokenNotFound
		}
		return TokenResult{}, fmt.Errorf("get verification code: %w", err)
	}
	if record.UsedAt.Valid {
		return TokenResult{}, ErrTokenUsed
	}
	if record.ExpiresAt.Time.Before(time.Now()) {
		return TokenResult{}, ErrTokenExpired
	}
	n, err := s.q.MarkVerificationCodeUsed(ctx, record.ID)
	if err != nil {
		return TokenResult{}, fmt.Errorf("mark code used: %w", err)
	}
	if n == 0 {
		return TokenResult{}, ErrTokenUsed
	}
	if err := s.q.SetEmailVerified(ctx, record.UserID); err != nil {
		return TokenResult{}, fmt.Errorf("set email verified: %w", err)
	}
	user, err := s.q.GetUserByID(ctx, record.UserID)
	if err != nil {
		return TokenResult{}, fmt.Errorf("get user: %w", err)
	}
	return s.issueTokenPair(ctx, user, meta)
}

// ChangePassword verifies the current password and replaces it with a new one.
// Every session except currentSessionID is revoked, so a password change kicks
// out anyone else who might be signed in to the account.
func (s *AuthService) ChangePassword(ctx context.Context, userID, currentSessionID uuid.UUID, currentPassword, newPassword string) error {
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}

	// A passwordless (Google-only) account has nothing to compare against.
	if !hasPassword(user) {
		return &ValidationError{Fields: map[string]string{"current_password": "no password is set on this account; you sign in with Google"}}
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(currentPassword)); err != nil {
		return ErrInvalidCredentials
	}

	if err := users.ValidatePassword(newPassword, s.cfg.PasswordMinLen); err != nil {
		return &ValidationError{Fields: map[string]string{"new_password": passwordProblem(err, s.cfg.PasswordMinLen)}}
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(newPassword), s.cfg.BCryptCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	if err := qtx.UpdatePasswordHash(ctx, db.UpdatePasswordHashParams{
		ID:           userID,
		PasswordHash: pgtype.Text{String: string(newHash), Valid: true},
	}); err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	if err := qtx.RevokeOtherUserRefreshTokens(ctx, db.RevokeOtherUserRefreshTokensParams{
		UserID: userID,
		ID:     currentSessionID,
	}); err != nil {
		return fmt.Errorf("revoke other sessions: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	s.notifyPasswordChanged(ctx, userID)
	return nil
}

// DeleteAccount permanently deletes the user after re-checking who is asking.
// A password user must supply their password. A passwordless (Google-only)
// user must type their account email instead, and the session making the
// request must have been created within recentSignInWindow, so a stolen
// long-lived session alone can't destroy the account.
// Households where they are the only member are deleted with them. Deletion is
// refused if it would leave a household that still has members without an
// admin; the user must promote someone else first.
func (s *AuthService) DeleteAccount(ctx context.Context, userID, currentSessionID uuid.UUID, password, confirmEmail string) error {
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if hasPassword(user) {
		if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash.String), []byte(password)); err != nil {
			return ErrInvalidCredentials
		}
	} else {
		if users.NormalizeEmail(confirmEmail) != user.Email {
			return &ValidationError{Fields: map[string]string{"email": "does not match your account email"}}
		}
		rt, err := s.q.GetRefreshTokenByID(ctx, currentSessionID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get session: %w", err)
		}
		if err != nil || rt.UserID != userID || !rt.CreatedAt.Valid || time.Since(rt.CreatedAt.Time) > recentSignInWindow {
			return &ValidationError{Fields: map[string]string{"email": "for your security, sign in with Google again, then delete your account within 10 minutes"}}
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	qtx := s.q.WithTx(tx)
	blocking, err := qtx.ListAdminHouseholdsWithOtherMembers(ctx, userID)
	if err != nil {
		return fmt.Errorf("check households: %w", err)
	}
	if len(blocking) > 0 {
		names := make([]string, len(blocking))
		for i, h := range blocking {
			names[i] = h.Name
		}
		return HouseholdBlockingError{Households: names}
	}
	if err := qtx.DeleteSoleMemberHouseholds(ctx, userID); err != nil {
		return fmt.Errorf("delete sole-member households: %w", err)
	}
	if err := qtx.DeleteUserByID(ctx, userID); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ResendVerification issues a fresh verification link if rate limit not exceeded.
func (s *AuthService) ResendVerification(ctx context.Context, userID uuid.UUID) error {
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if user.EmailVerified {
		return ErrEmailAlreadyVerified
	}

	count, err := s.q.CountRecentVerificationCodesByUserID(ctx, userID)
	if err != nil {
		return fmt.Errorf("count codes: %w", err)
	}
	if count >= 3 {
		oldest, oldestErr := s.q.GetOldestRecentVerificationCode(ctx, userID)
		retryAt := time.Now().Add(time.Hour)
		if oldestErr == nil && oldest.Valid {
			retryAt = oldest.Time.Add(time.Hour)
		}
		return RateLimitedError{RetryAt: retryAt}
	}

	return s.sendVerificationEmail(ctx, user)
}

// ── private helpers ───────────────────────────────────────────────────────────

func (s *AuthService) issueTokenPair(ctx context.Context, user db.User, meta DeviceMeta) (TokenResult, error) {
	rawRefresh, hashedRefresh, err := auth.GenerateRefreshToken()
	if err != nil {
		return TokenResult{}, fmt.Errorf("generate refresh token: %w", err)
	}

	sessionID, err := uuid.NewV7()
	if err != nil {
		return TokenResult{}, fmt.Errorf("generate token id: %w", err)
	}
	expiresAt := time.Now().Add(s.cfg.RefreshExpiry)

	_, err = s.q.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		ID:         sessionID,
		UserID:     user.ID,
		TokenHash:  hashedRefresh,
		DeviceName: pgtype.Text{String: meta.DeviceName, Valid: meta.DeviceName != ""},
		IpAddress:  meta.IPAddress,
		UserAgent:  pgtype.Text{String: meta.UserAgent, Valid: meta.UserAgent != ""},
		ExpiresAt:  pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if err != nil {
		return TokenResult{}, fmt.Errorf("insert refresh token: %w", err)
	}

	tp, err := auth.IssueAccessToken(user.ID, sessionID, s.cfg.JWTSecret, s.cfg.AccessExpiry)
	if err != nil {
		return TokenResult{}, fmt.Errorf("issue access token: %w", err)
	}

	return TokenResult{
		AccessToken:        tp.AccessToken,
		AccessTokenExpiry:  tp.ExpiresAt,
		RefreshToken:       rawRefresh,
		RefreshTokenExpiry: expiresAt,
		User:               user,
	}, nil
}

func (s *AuthService) sendVerificationEmail(ctx context.Context, user db.User) error {
	rawToken, hashedToken, err := auth.GenerateVerificationToken()
	if err != nil {
		return fmt.Errorf("generate verification token: %w", err)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate code id: %w", err)
	}
	_, err = s.q.InsertEmailVerificationCode(ctx, db.InsertEmailVerificationCodeParams{
		ID:        id,
		UserID:    user.ID,
		TokenHash: hashedToken,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("insert verification code: %w", err)
	}

	verifyURL := fmt.Sprintf("%s/verify-email?token=%s", s.cfg.AppBaseURL, rawToken)
	subj, htmlBody, text := email.VerificationEmail(s.cfg.AppName, verifyURL)
	return s.mailer.Send(user.Email, subj, htmlBody, text)
}

func (s *AuthService) notifyPasswordChanged(ctx context.Context, userID uuid.UUID) {
	user, err := s.q.GetUserByID(ctx, userID)
	if err != nil {
		slog.Error("get user after password change", "user_id", userID, "error", err)
		return
	}
	subj, html, text := email.PasswordChangedEmail(s.cfg.AppName)
	s.goSend(func() {
		if sendErr := s.mailer.Send(user.Email, subj, html, text); sendErr != nil {
			slog.Error("send password changed email", "error", sendErr, "user_id", user.ID)
		}
	})
}

// passwordProblem converts a users.ValidatePassword error into a field message.
func passwordProblem(err error, minLen int) string {
	if errors.Is(err, users.ErrPasswordTooLong) {
		return "must be at most 72 bytes"
	}
	return fmt.Sprintf("must be at least %d characters", minLen)
}

// isUniqueViolation reports whether err is a Postgres unique-constraint violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
