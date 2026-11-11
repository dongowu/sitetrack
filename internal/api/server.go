package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/auth"
	"github.com/dongowu/sitetrack/internal/config"
	"github.com/dongowu/sitetrack/internal/httpx"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Server struct {
	db     *gorm.DB
	cfg    config.Config
	router *gin.Engine
}

func NewServer(db *gorm.DB, cfg config.Config) *Server {
	// Gin's own logger and recovery are replaced below: the logger would print a
	// second line per request next to the structured one, and the default
	// recovery writes a plain-text 500 that breaks the error envelope.
	gin.DefaultWriter = ginWriter{}
	gin.DefaultErrorWriter = ginWriter{}
	gin.SetMode(gin.ReleaseMode)
	// A typo in a JSON body is a 400, not a silently ignored field.
	binding.EnableDecoderDisallowUnknownFields = true

	s := &Server{db: db, cfg: cfg, router: gin.New()}
	s.router.Use(s.recoverer(), s.requestLogger(), s.cors())
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.router
}

// ginWriter swallows what Gin prints directly so structured logs stay the only
// output. Anything Gin would have written has already been logged by the
// middleware with more context.
type ginWriter struct{}

func (ginWriter) Write(p []byte) (int, error) { return len(p), nil }

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

func (s *Server) routes() {
	r := s.router
	const p = "/api/v1"

	// public
	r.POST(p+"/auth/register", s.handleRegister)
	r.POST(p+"/auth/login", s.handleLogin)
	r.POST(p+"/auth/refresh", s.handleRefresh)
	r.GET(p+"/health", s.handleHealth)

	// Everything below needs a live session. Applied per route rather than via a
	// group because the public routes above sit under the same prefix.
	authed := func(h gin.HandlerFunc) []gin.HandlerFunc {
		return []gin.HandlerFunc{s.requireAuth(), h}
	}
	route := func(method, path string, h gin.HandlerFunc) {
		r.Handle(method, path, authed(h)...)
	}

	route(http.MethodPost, p+"/auth/logout", s.handleLogout)
	route(http.MethodGet, p+"/me", s.handleMe)
	route(http.MethodPatch, p+"/me", s.handleUpdateMe)
	route(http.MethodPost, p+"/me/password", s.handleChangePassword)

	route(http.MethodGet, p+"/notifications", s.handleListNotifications)
	route(http.MethodPost, p+"/notifications/read", s.handleMarkNotificationsRead)
	route(http.MethodPost, p+"/invitations/:invitationID/accept", s.handleAcceptInvitation)
	route(http.MethodPost, p+"/invitations/:invitationID/reject", s.handleRejectInvitation)

	route(http.MethodGet, p+"/materials", s.handleListMaterials)
	route(http.MethodPost, p+"/materials", s.handleCreateMaterial)
	route(http.MethodPatch, p+"/materials/:materialID", s.handleUpdateMaterial)
	route(http.MethodDelete, p+"/materials/:materialID", s.handleDeleteMaterial)

	route(http.MethodGet, p+"/users", s.handleSearchUsers)

	route(http.MethodPost, p+"/projects", s.handleCreateProject)
	route(http.MethodGet, p+"/projects", s.handleListProjects)
	route(http.MethodGet, p+"/projects/:projectCode", s.handleGetProject)
	route(http.MethodPatch, p+"/projects/:projectCode", s.handleUpdateProject)
	route(http.MethodDelete, p+"/projects/:projectCode", s.handleDeleteProject)

	route(http.MethodGet, p+"/projects/:projectCode/members", s.handleListMembers)
	route(http.MethodPatch, p+"/projects/:projectCode/members/:userID", s.handleUpdateMember)
	route(http.MethodDelete, p+"/projects/:projectCode/members/:userID", s.handleRemoveMember)
	route(http.MethodPost, p+"/projects/:projectCode/invitations", s.handleInvite)
	route(http.MethodDelete, p+"/projects/:projectCode/invitations/:invitationID", s.handleWithdrawInvitation)

	route(http.MethodGet, p+"/projects/:projectCode/tasks", s.handleListTasks)
	route(http.MethodPost, p+"/projects/:projectCode/tasks", s.handleCreateTask)
	route(http.MethodGet, p+"/projects/:projectCode/tasks/:taskCode", s.handleGetTask)
	route(http.MethodPatch, p+"/projects/:projectCode/tasks/:taskCode", s.handleUpdateTask)
	route(http.MethodDelete, p+"/projects/:projectCode/tasks/:taskCode", s.handleDeleteTask)
	route(http.MethodPost, p+"/projects/:projectCode/tasks/:taskCode/start", s.handleTaskStart)
	route(http.MethodPost, p+"/projects/:projectCode/tasks/:taskCode/done", s.handleTaskDone)
	route(http.MethodPost, p+"/projects/:projectCode/tasks/:taskCode/reopen", s.handleTaskReopen)
	route(http.MethodGet, p+"/projects/:projectCode/tasks/:taskCode/dependencies", s.handleGetTaskGraph)
	route(http.MethodPost, p+"/projects/:projectCode/tasks/:taskCode/dependencies", s.handleAddDependency)
	route(http.MethodDelete, p+"/projects/:projectCode/tasks/:taskCode/dependencies/:dependsOnCode", s.handleRemoveDependency)

	route(http.MethodGet, p+"/projects/:projectCode/store", s.handleProjectStore)
	route(http.MethodGet, p+"/projects/:projectCode/deliveries", s.handleProjectDeliveries)
	route(http.MethodPost, p+"/projects/:projectCode/deliveries", s.handleProjectCreateDelivery)
	route(http.MethodGet, p+"/projects/:projectCode/deliveries/:deliveryID", s.handleGetProjectDelivery)
	route(http.MethodPost, p+"/projects/:projectCode/deliveries/:deliveryID/record", s.handleProjectDeliveryRecord)
	route(http.MethodPost, p+"/projects/:projectCode/deliveries/:deliveryID/reject", s.handleProjectDeliveryReject)
	route(http.MethodPost, p+"/projects/:projectCode/deliveries/:deliveryID/cancel", s.handleProjectDeliveryCancel)

	route(http.MethodGet, p+"/company/store", s.handleCompanyStore)
	route(http.MethodPost, p+"/company/store/stock", s.handleAddStock)
	route(http.MethodPatch, p+"/company/store/stock/:materialID", s.handleSetReorderPoint)
	route(http.MethodGet, p+"/company/deliveries", s.handleCompanyDeliveries)
	route(http.MethodPost, p+"/company/deliveries", s.handleCompanyCreateDelivery)
	route(http.MethodGet, p+"/company/deliveries/:deliveryID", s.handleGetCompanyDelivery)
	route(http.MethodPost, p+"/company/deliveries/:deliveryID/record", s.handleCompanyDeliveryRecord)
	route(http.MethodPost, p+"/company/deliveries/:deliveryID/reject", s.handleCompanyDeliveryReject)
	route(http.MethodPost, p+"/company/deliveries/:deliveryID/cancel", s.handleCompanyDeliveryCancel)

	r.NoRoute(func(c *gin.Context) {
		httpx.Fail(c, httpx.NotFound("no route for %s %s", c.Request.Method, c.Request.URL.Path))
	})
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

const principalKey = "sitetrack.principal"

// Principal is the authenticated caller, refreshed from the database on every
// request so a revoked session or a changed role is effective on the next call.
type Principal struct {
	UserID    uuid.UUID
	Name      string
	Email     string
	Role      string
	SessionID uuid.UUID
}

func (p *Principal) IsSuperAdmin() bool { return p.Role == auth.RoleSuperAdmin }

func (p *Principal) CanUseCompanyStore() bool {
	return p.Role == auth.RoleStoreManager || p.Role == auth.RoleSuperAdmin
}

type principalRow struct {
	ID        uuid.UUID
	Name      string
	Email     string
	AppRole   string
	SessionID uuid.UUID
}

func (s *Server) requireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := strings.CutPrefix(c.GetHeader("Authorization"), "Bearer ")
		if !ok || raw == "" {
			httpx.Fail(c, httpx.Unauthorized("missing bearer token"))
			return
		}
		claims, err := auth.ParseAccess(s.cfg.JWTSecret, raw)
		if err != nil {
			httpx.Fail(c, httpx.Unauthorized("invalid or expired access token"))
			return
		}

		// One round trip: is the session still alive, and who is the user now?
		var row principalRow
		err = s.db.WithContext(c.Request.Context()).
			Table("sessions").
			Select("users.id, users.name, users.email, users.app_role, sessions.id AS session_id").
			Joins("JOIN users ON users.id = sessions.user_id").
			Where("sessions.id = ? AND sessions.user_id = ?", claims.SessionID, claims.UserID).
			Where("sessions.revoked_at IS NULL AND sessions.expires_at > now() AND users.deleted_at IS NULL").
			Take(&row).Error
		if err != nil {
			httpx.Fail(c, httpx.Unauthorized("session is no longer valid"))
			return
		}

		c.Set(principalKey, &Principal{
			UserID:    row.ID,
			Name:      row.Name,
			Email:     row.Email,
			Role:      row.AppRole,
			SessionID: row.SessionID,
		})
		c.Next()
	}
}

func caller(c *gin.Context) *Principal {
	v, ok := c.Get(principalKey)
	if !ok {
		return nil
	}
	p, _ := v.(*Principal)
	return p
}

func (s *Server) recoverer() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, rec any) {
		slog.Error("panic",
			"rec", rec,
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"stack", string(debug.Stack()))
		httpx.Fail(c, httpx.Internal(errors.New("panic")))
	})
}

func (s *Server) requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if strings.HasPrefix(c.Request.URL.Path, "/api/") {
			slog.Info("request",
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"status", c.Writer.Status(),
				"dur_ms", time.Since(start).Milliseconds())
		}
	}
}

// cors keeps the frontend team unblocked during development.
func (s *Server) cors() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Max-Age", "86400")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// decodeJSON binds a JSON body, capped at 1 MiB and rejecting unknown fields so
// a misspelled key is reported instead of ignored.
func decodeJSON(c *gin.Context, dst any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindWith(dst, binding.JSON); err != nil {
		return httpx.BadRequest("invalid JSON body: %v", err)
	}
	return nil
}

type paging struct {
	Limit  int
	Offset int
}

func (s *Server) paging(c *gin.Context) paging {
	limit := 20
	if v, err := strconv.Atoi(c.Query("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > s.cfg.MaxPageSize {
		limit = s.cfg.MaxPageSize
	}
	offset := 0
	if v, err := strconv.Atoi(c.Query("offset")); err == nil && v > 0 {
		offset = v
	}
	return paging{Limit: limit, Offset: offset}
}

func queryParam(c *gin.Context, key string) string {
	return strings.TrimSpace(c.Query(key))
}

func boolParam(c *gin.Context, key string) bool {
	switch strings.ToLower(queryParam(c, key)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// dateParam parses YYYY-MM-DD. An empty string yields nil.
func dateParam(c *gin.Context, key string) (*time.Time, error) {
	raw := queryParam(c, key)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.DateOnly, raw)
	if err != nil {
		return nil, httpx.BadRequest("%s must be a date in YYYY-MM-DD form", key)
	}
	return &t, nil
}

// likePattern turns user input into a safe ILIKE pattern. Without escaping, a
// search for "100%" would silently match everything.
func likePattern(term string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(strings.ToLower(term)) + "%"
}

func direction(dir string) string {
	if strings.EqualFold(dir, "asc") {
		return "ASC"
	}
	return "DESC"
}

// ordering builds a safe ORDER BY clause. The expression always comes from a
// whitelist, never from raw user input, and clause.Expr is used instead of a
// plain string so GORM does not split on the commas inside CASE expressions.
func ordering(expr, dir string) clause.OrderBy {
	return clause.OrderBy{Expression: clause.Expr{SQL: expr + " " + dir, WithoutParentheses: true}}
}

// unreadCount is included in the meta of every list response so the shell can
// show the notification badge without an extra round trip.
func (s *Server) unreadCount(ctx context.Context, userID uuid.UUID) int {
	var n int64
	err := s.db.WithContext(ctx).Table("invitations").
		Where("invitee_id = ? AND read_at IS NULL AND status = 'pending'", userID).
		Count(&n).Error
	if err != nil {
		return 0
	}
	return int(n)
}

func parseUUID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, httpx.BadRequest("invalid identifier")
	}
	return id, nil
}

func isNotFound(err error) bool { return errors.Is(err, gorm.ErrRecordNotFound) }
