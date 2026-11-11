package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/auth"
	"github.com/dongowu/sitetrack/internal/database"
	"github.com/dongowu/sitetrack/internal/httpx"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type userDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	AppRole   string    `json:"app_role"`
	CreatedAt time.Time `json:"created_at"`
}

func toUserDTO(u database.User) userDTO {
	return userDTO{ID: u.ID.String(), Name: u.Name, Email: u.Email, AppRole: u.AppRole, CreatedAt: u.CreatedAt}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	httpx.OK(w, map[string]string{"status": "ok"})
}

type registerRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	AppRole  string `json:"app_role"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.AppRole = strings.ToLower(strings.TrimSpace(req.AppRole))
	if req.AppRole == "" {
		req.AppRole = auth.RoleUser
	}

	if req.Name == "" {
		httpx.Fail(w, r, httpx.BadRequest("name is required"))
		return
	}
	if !looksLikeEmail(req.Email) {
		httpx.Fail(w, r, httpx.BadRequest("a valid email is required"))
		return
	}
	if len(req.Password) < 8 {
		httpx.Fail(w, r, httpx.BadRequest("password must be at least 8 characters"))
		return
	}
	if !auth.ValidRole(req.AppRole) {
		httpx.Fail(w, r, httpx.BadRequest("app_role must be one of user, store_manager, superadmin"))
		return
	}

	hash, err := auth.HashPassword(req.Password, s.cfg.BcryptCost)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	u := database.User{
		ID:           uuid.New(),
		Name:         req.Name,
		Email:        req.Email,
		PasswordHash: hash,
		AppRole:      req.AppRole,
		CreatedAt:    time.Now().UTC(),
	}
	if err := s.db.WithContext(r.Context()).Create(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			httpx.Fail(w, r, httpx.Conflict("that email is already registered"))
			return
		}
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	tokens, err := s.startSession(r.Context(), u.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.Created(w, map[string]any{
		"user":          toUserDTO(u),
		"access_token":  tokens.AccessToken,
		"refresh_token": tokens.RefreshToken,
		"expires_in":    tokens.ExpiresIn,
	})
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// dummyHash keeps the failed-login path the same cost as the success path, so
// response time cannot be used to probe whether an email is registered.
var dummyHash, _ = auth.HashPassword("sitetrack-dummy-password", 12)

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	var u database.User
	err := s.db.WithContext(r.Context()).
		Where("lower(email) = ? AND deleted_at IS NULL", email).
		Take(&u).Error
	if isNotFound(err) {
		auth.CheckPassword(dummyHash, req.Password)
		httpx.Fail(w, r, httpx.Unauthorized("email or password is incorrect"))
		return
	}
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	if !auth.CheckPassword(u.PasswordHash, req.Password) {
		httpx.Fail(w, r, httpx.Unauthorized("email or password is incorrect"))
		return
	}

	tokens, err := s.startSession(r.Context(), u.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.OK(w, map[string]any{
		"user":          toUserDTO(u),
		"access_token":  tokens.AccessToken,
		"refresh_token": tokens.RefreshToken,
		"expires_in":    tokens.ExpiresIn,
	})
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// handleRefresh rotates the refresh token. The session id stays the same, so a
// logout still kills the pair no matter how many times it was refreshed.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.RefreshToken == "" {
		httpx.Fail(w, r, httpx.BadRequest("refresh_token is required"))
		return
	}

	var session database.Session
	err := s.db.WithContext(r.Context()).
		Where("refresh_token_hash = ? AND revoked_at IS NULL AND expires_at > now()", auth.HashRefresh(req.RefreshToken)).
		Take(&session).Error
	if err != nil {
		httpx.Fail(w, r, httpx.Unauthorized("refresh token is invalid or expired"))
		return
	}

	// The user must still exist and still be active.
	var active int64
	if err := s.db.WithContext(r.Context()).Table("users").
		Where("id = ? AND deleted_at IS NULL", session.UserID).Count(&active).Error; err != nil || active == 0 {
		httpx.Fail(w, r, httpx.Unauthorized("refresh token is invalid or expired"))
		return
	}

	fresh, err := auth.NewTokens(s.cfg.JWTSecret, s.cfg.AccessTokenTTL)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	access, err := auth.SignAccess(s.cfg.JWTSecret, s.cfg.AccessTokenTTL, session.UserID.String(), session.ID.String())
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	err = s.db.WithContext(r.Context()).Model(&database.Session{}).
		Where("id = ?", session.ID).
		Updates(map[string]any{
			"refresh_token_hash": auth.HashRefresh(fresh.RefreshToken),
			"last_used_at":       time.Now().UTC(),
			"expires_at":         time.Now().UTC().Add(s.cfg.RefreshTokenTTL),
		}).Error
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.OK(w, map[string]any{
		"access_token":  access,
		"refresh_token": fresh.RefreshToken,
		"expires_in":    fresh.ExpiresIn,
	})
}

// handleLogout revokes exactly the session the caller used. The middleware
// checks sessions.revoked_at on every request, so the next call with the same
// access token is already rejected.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	if err := s.db.WithContext(r.Context()).Model(&database.Session{}).
		Where("id = ? AND revoked_at IS NULL", p.SessionID).
		Update("revoked_at", time.Now().UTC()).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.NoContent(w)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	ctx := r.Context()

	var u database.User
	if err := s.db.WithContext(ctx).Where("id = ?", p.UserID).Take(&u).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	var projects, pending int64
	s.db.WithContext(ctx).Model(&database.ProjectMember{}).Where("user_id = ?", p.UserID).Count(&projects)
	s.db.WithContext(ctx).Model(&database.Invitation{}).
		Where("invitee_id = ? AND status = 'pending'", p.UserID).Count(&pending)

	httpx.OK(w, map[string]any{
		"user":                 toUserDTO(u),
		"project_count":        projects,
		"pending_invitations":  pending,
		"unread_notifications": s.unreadCount(ctx, p.UserID),
	})
}

type updateMeRequest struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	var req updateMeRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	updates := map[string]any{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			httpx.Fail(w, r, httpx.BadRequest("name cannot be empty"))
			return
		}
		updates["name"] = name
	}
	if req.Email != nil {
		email := strings.ToLower(strings.TrimSpace(*req.Email))
		if !looksLikeEmail(email) {
			httpx.Fail(w, r, httpx.BadRequest("a valid email is required"))
			return
		}
		updates["email"] = email
	}
	if len(updates) == 0 {
		httpx.Fail(w, r, httpx.BadRequest("nothing to update"))
		return
	}

	if err := s.db.WithContext(r.Context()).Model(&database.User{}).
		Where("id = ?", p.UserID).Updates(updates).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			httpx.Fail(w, r, httpx.Conflict("that email is already registered"))
			return
		}
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	s.handleMe(w, r)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword revokes every other session: a password change is the one
// moment where logging other devices out is expected.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	var req changePasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if len(req.NewPassword) < 8 {
		httpx.Fail(w, r, httpx.BadRequest("new_password must be at least 8 characters"))
		return
	}

	var u database.User
	if err := s.db.WithContext(r.Context()).Where("id = ?", p.UserID).Take(&u).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	if !auth.CheckPassword(u.PasswordHash, req.CurrentPassword) {
		httpx.Fail(w, r, httpx.Unauthorized("current password is incorrect"))
		return
	}

	hash, err := auth.HashPassword(req.NewPassword, s.cfg.BcryptCost)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&database.User{}).Where("id = ?", p.UserID).
			Update("password_hash", hash).Error; err != nil {
			return err
		}
		return tx.Model(&database.Session{}).
			Where("user_id = ? AND id <> ? AND revoked_at IS NULL", p.UserID, p.SessionID).
			Update("revoked_at", time.Now().UTC()).Error
	})
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.NoContent(w)
}

// startSession inserts the session row first, then signs an access token that
// points at it.
func (s *Server) startSession(ctx context.Context, userID uuid.UUID) (auth.Tokens, error) {
	tokens, err := auth.NewTokens(s.cfg.JWTSecret, s.cfg.AccessTokenTTL)
	if err != nil {
		return tokens, err
	}

	now := time.Now().UTC()
	session := database.Session{
		ID:               uuid.New(),
		UserID:           userID,
		RefreshTokenHash: auth.HashRefresh(tokens.RefreshToken),
		CreatedAt:        now,
		LastUsedAt:       now,
		ExpiresAt:        now.Add(s.cfg.RefreshTokenTTL),
	}
	if err := s.db.WithContext(ctx).Create(&session).Error; err != nil {
		return tokens, err
	}

	tokens.AccessToken, err = auth.SignAccess(s.cfg.JWTSecret, s.cfg.AccessTokenTTL, userID.String(), session.ID.String())
	return tokens, err
}

func looksLikeEmail(v string) bool {
	at := strings.IndexByte(v, '@')
	if at <= 0 || at == len(v)-1 || len(v) > 254 {
		return false
	}
	domain := v[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return false
	}
	return !strings.ContainsAny(v, " \t\r\n")
}
