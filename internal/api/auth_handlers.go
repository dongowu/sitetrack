package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/auth"
	"github.com/dongowu/sitetrack/internal/database"
	"github.com/dongowu/sitetrack/internal/httpx"
	"github.com/gin-gonic/gin"
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

func (s *Server) handleHealth(c *gin.Context) {
	httpx.OK(c, map[string]string{"status": "ok"})
}

type registerRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
	AppRole  string `json:"app_role"`
}

func (s *Server) handleRegister(c *gin.Context) {
	var req registerRequest
	if err := decodeJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.AppRole = strings.ToLower(strings.TrimSpace(req.AppRole))
	if req.AppRole == "" {
		req.AppRole = auth.RoleUser
	}

	if req.Name == "" {
		httpx.Fail(c, httpx.BadRequest("name is required"))
		return
	}
	if !looksLikeEmail(req.Email) {
		httpx.Fail(c, httpx.BadRequest("a valid email is required"))
		return
	}
	if len(req.Password) < 8 {
		httpx.Fail(c, httpx.BadRequest("password must be at least 8 characters"))
		return
	}
	if !auth.ValidRole(req.AppRole) {
		httpx.Fail(c, httpx.BadRequest("app_role must be one of user, store_manager, superadmin"))
		return
	}

	hash, err := auth.HashPassword(req.Password, s.cfg.BcryptCost)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
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
	if err := s.db.WithContext(c.Request.Context()).Create(&u).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			httpx.Fail(c, httpx.Conflict("that email is already registered"))
			return
		}
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	tokens, err := s.startSession(c.Request.Context(), u.ID)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	httpx.Created(c, map[string]any{
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

func (s *Server) handleLogin(c *gin.Context) {
	var req loginRequest
	if err := decodeJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))

	var u database.User
	err := s.db.WithContext(c.Request.Context()).
		Where("lower(email) = ? AND deleted_at IS NULL", email).
		Take(&u).Error
	if isNotFound(err) {
		auth.CheckPassword(dummyHash, req.Password)
		httpx.Fail(c, httpx.Unauthorized("email or password is incorrect"))
		return
	}
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if !auth.CheckPassword(u.PasswordHash, req.Password) {
		httpx.Fail(c, httpx.Unauthorized("email or password is incorrect"))
		return
	}

	tokens, err := s.startSession(c.Request.Context(), u.ID)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	httpx.OK(c, map[string]any{
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
func (s *Server) handleRefresh(c *gin.Context) {
	var req refreshRequest
	if err := decodeJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	if req.RefreshToken == "" {
		httpx.Fail(c, httpx.BadRequest("refresh_token is required"))
		return
	}

	var session database.Session
	err := s.db.WithContext(c.Request.Context()).
		Where("refresh_token_hash = ? AND revoked_at IS NULL AND expires_at > now()", auth.HashRefresh(req.RefreshToken)).
		Take(&session).Error
	if err != nil {
		httpx.Fail(c, httpx.Unauthorized("refresh token is invalid or expired"))
		return
	}

	// The user must still exist and still be active.
	var active int64
	if err := s.db.WithContext(c.Request.Context()).Table("users").
		Where("id = ? AND deleted_at IS NULL", session.UserID).Count(&active).Error; err != nil || active == 0 {
		httpx.Fail(c, httpx.Unauthorized("refresh token is invalid or expired"))
		return
	}

	fresh, err := auth.NewTokens(s.cfg.JWTSecret, s.cfg.AccessTokenTTL)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	access, err := auth.SignAccess(s.cfg.JWTSecret, s.cfg.AccessTokenTTL, session.UserID.String(), session.ID.String())
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	err = s.db.WithContext(c.Request.Context()).Model(&database.Session{}).
		Where("id = ?", session.ID).
		Updates(map[string]any{
			"refresh_token_hash": auth.HashRefresh(fresh.RefreshToken),
			"last_used_at":       time.Now().UTC(),
			"expires_at":         time.Now().UTC().Add(s.cfg.RefreshTokenTTL),
		}).Error
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	httpx.OK(c, map[string]any{
		"access_token":  access,
		"refresh_token": fresh.RefreshToken,
		"expires_in":    fresh.ExpiresIn,
	})
}

// handleLogout revokes exactly the session the caller used. The middleware
// checks sessions.revoked_at on every request, so the next call with the same
// access token is already rejected.
func (s *Server) handleLogout(c *gin.Context) {
	p := caller(c)
	if err := s.db.WithContext(c.Request.Context()).Model(&database.Session{}).
		Where("id = ? AND revoked_at IS NULL", p.SessionID).
		Update("revoked_at", time.Now().UTC()).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.NoContent(c)
}

func (s *Server) handleMe(c *gin.Context) {
	p := caller(c)
	ctx := c.Request.Context()

	var u database.User
	if err := s.db.WithContext(ctx).Where("id = ?", p.UserID).Take(&u).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	var projects, pending int64
	s.db.WithContext(ctx).Model(&database.ProjectMember{}).Where("user_id = ?", p.UserID).Count(&projects)
	s.db.WithContext(ctx).Model(&database.Invitation{}).
		Where("invitee_id = ? AND status = 'pending'", p.UserID).Count(&pending)

	httpx.OK(c, map[string]any{
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

func (s *Server) handleUpdateMe(c *gin.Context) {
	p := caller(c)
	var req updateMeRequest
	if err := decodeJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}

	updates := map[string]any{}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			httpx.Fail(c, httpx.BadRequest("name cannot be empty"))
			return
		}
		updates["name"] = name
	}
	if req.Email != nil {
		email := strings.ToLower(strings.TrimSpace(*req.Email))
		if !looksLikeEmail(email) {
			httpx.Fail(c, httpx.BadRequest("a valid email is required"))
			return
		}
		updates["email"] = email
	}
	if len(updates) == 0 {
		httpx.Fail(c, httpx.BadRequest("nothing to update"))
		return
	}

	if err := s.db.WithContext(c.Request.Context()).Model(&database.User{}).
		Where("id = ?", p.UserID).Updates(updates).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			httpx.Fail(c, httpx.Conflict("that email is already registered"))
			return
		}
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	s.handleMe(c)
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword revokes every other session: a password change is the one
// moment where logging other devices out is expected.
func (s *Server) handleChangePassword(c *gin.Context) {
	p := caller(c)
	var req changePasswordRequest
	if err := decodeJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	if len(req.NewPassword) < 8 {
		httpx.Fail(c, httpx.BadRequest("new_password must be at least 8 characters"))
		return
	}

	var u database.User
	if err := s.db.WithContext(c.Request.Context()).Where("id = ?", p.UserID).Take(&u).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	if !auth.CheckPassword(u.PasswordHash, req.CurrentPassword) {
		httpx.Fail(c, httpx.Unauthorized("current password is incorrect"))
		return
	}

	hash, err := auth.HashPassword(req.NewPassword, s.cfg.BcryptCost)
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	err = s.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&database.User{}).Where("id = ?", p.UserID).
			Update("password_hash", hash).Error; err != nil {
			return err
		}
		return tx.Model(&database.Session{}).
			Where("user_id = ? AND id <> ? AND revoked_at IS NULL", p.UserID, p.SessionID).
			Update("revoked_at", time.Now().UTC()).Error
	})
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.NoContent(c)
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
