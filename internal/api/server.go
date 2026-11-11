package api

import (
	"context"
	"encoding/json"
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
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Server struct {
	db  *gorm.DB
	cfg config.Config
	mux *http.ServeMux
}

func NewServer(db *gorm.DB, cfg config.Config) *Server {
	s := &Server{db: db, cfg: cfg, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.recoverer(s.requestLogger(s.cors(s.mux)))
}

// ---------------------------------------------------------------------------
// routing
// ---------------------------------------------------------------------------

func (s *Server) routes() {
	const p = "/api/v1"

	// public
	s.mux.HandleFunc("POST "+p+"/auth/register", s.handleRegister)
	s.mux.HandleFunc("POST "+p+"/auth/login", s.handleLogin)
	s.mux.HandleFunc("POST "+p+"/auth/refresh", s.handleRefresh)
	s.mux.HandleFunc("GET "+p+"/health", s.handleHealth)

	// authenticated
	s.mux.HandleFunc("POST "+p+"/auth/logout", s.auth(s.handleLogout))
	s.mux.HandleFunc("GET "+p+"/me", s.auth(s.handleMe))
	s.mux.HandleFunc("PATCH "+p+"/me", s.auth(s.handleUpdateMe))
	s.mux.HandleFunc("POST "+p+"/me/password", s.auth(s.handleChangePassword))

	s.mux.HandleFunc("GET "+p+"/notifications", s.auth(s.handleListNotifications))
	s.mux.HandleFunc("POST "+p+"/notifications/read", s.auth(s.handleMarkNotificationsRead))
	s.mux.HandleFunc("POST "+p+"/invitations/{invitationID}/accept", s.auth(s.handleAcceptInvitation))
	s.mux.HandleFunc("POST "+p+"/invitations/{invitationID}/reject", s.auth(s.handleRejectInvitation))

	s.mux.HandleFunc("GET "+p+"/materials", s.auth(s.handleListMaterials))
	s.mux.HandleFunc("POST "+p+"/materials", s.auth(s.handleCreateMaterial))
	s.mux.HandleFunc("PATCH "+p+"/materials/{materialID}", s.auth(s.handleUpdateMaterial))
	s.mux.HandleFunc("DELETE "+p+"/materials/{materialID}", s.auth(s.handleDeleteMaterial))

	s.mux.HandleFunc("GET "+p+"/users", s.auth(s.handleSearchUsers))

	s.mux.HandleFunc("POST "+p+"/projects", s.auth(s.handleCreateProject))
	s.mux.HandleFunc("GET "+p+"/projects", s.auth(s.handleListProjects))
	s.mux.HandleFunc("GET "+p+"/projects/{projectCode}", s.auth(s.handleGetProject))
	s.mux.HandleFunc("PATCH "+p+"/projects/{projectCode}", s.auth(s.handleUpdateProject))
	s.mux.HandleFunc("DELETE "+p+"/projects/{projectCode}", s.auth(s.handleDeleteProject))

	s.mux.HandleFunc("GET "+p+"/projects/{projectCode}/members", s.auth(s.handleListMembers))
	s.mux.HandleFunc("PATCH "+p+"/projects/{projectCode}/members/{userID}", s.auth(s.handleUpdateMember))
	s.mux.HandleFunc("DELETE "+p+"/projects/{projectCode}/members/{userID}", s.auth(s.handleRemoveMember))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/invitations", s.auth(s.handleInvite))
	s.mux.HandleFunc("DELETE "+p+"/projects/{projectCode}/invitations/{invitationID}", s.auth(s.handleWithdrawInvitation))

	s.mux.HandleFunc("GET "+p+"/projects/{projectCode}/tasks", s.auth(s.handleListTasks))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/tasks", s.auth(s.handleCreateTask))
	s.mux.HandleFunc("GET "+p+"/projects/{projectCode}/tasks/{taskCode}", s.auth(s.handleGetTask))
	s.mux.HandleFunc("PATCH "+p+"/projects/{projectCode}/tasks/{taskCode}", s.auth(s.handleUpdateTask))
	s.mux.HandleFunc("DELETE "+p+"/projects/{projectCode}/tasks/{taskCode}", s.auth(s.handleDeleteTask))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/tasks/{taskCode}/start", s.auth(s.handleTaskStart))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/tasks/{taskCode}/done", s.auth(s.handleTaskDone))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/tasks/{taskCode}/reopen", s.auth(s.handleTaskReopen))
	s.mux.HandleFunc("GET "+p+"/projects/{projectCode}/tasks/{taskCode}/dependencies", s.auth(s.handleGetTaskGraph))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/tasks/{taskCode}/dependencies", s.auth(s.handleAddDependency))
	s.mux.HandleFunc("DELETE "+p+"/projects/{projectCode}/tasks/{taskCode}/dependencies/{dependsOnCode}", s.auth(s.handleRemoveDependency))

	s.mux.HandleFunc("GET "+p+"/projects/{projectCode}/store", s.auth(s.handleProjectStore))
	s.mux.HandleFunc("GET "+p+"/projects/{projectCode}/deliveries", s.auth(s.handleProjectDeliveries))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/deliveries", s.auth(s.handleProjectCreateDelivery))
	s.mux.HandleFunc("GET "+p+"/projects/{projectCode}/deliveries/{deliveryID}", s.auth(s.handleGetProjectDelivery))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/deliveries/{deliveryID}/record", s.auth(s.handleProjectDeliveryRecord))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/deliveries/{deliveryID}/reject", s.auth(s.handleProjectDeliveryReject))
	s.mux.HandleFunc("POST "+p+"/projects/{projectCode}/deliveries/{deliveryID}/cancel", s.auth(s.handleProjectDeliveryCancel))

	s.mux.HandleFunc("GET "+p+"/company/store", s.auth(s.handleCompanyStore))
	s.mux.HandleFunc("POST "+p+"/company/store/stock", s.auth(s.handleAddStock))
	s.mux.HandleFunc("PATCH "+p+"/company/store/stock/{materialID}", s.auth(s.handleSetReorderPoint))
	s.mux.HandleFunc("GET "+p+"/company/deliveries", s.auth(s.handleCompanyDeliveries))
	s.mux.HandleFunc("POST "+p+"/company/deliveries", s.auth(s.handleCompanyCreateDelivery))
	s.mux.HandleFunc("GET "+p+"/company/deliveries/{deliveryID}", s.auth(s.handleGetCompanyDelivery))
	s.mux.HandleFunc("POST "+p+"/company/deliveries/{deliveryID}/record", s.auth(s.handleCompanyDeliveryRecord))
	s.mux.HandleFunc("POST "+p+"/company/deliveries/{deliveryID}/reject", s.auth(s.handleCompanyDeliveryReject))
	s.mux.HandleFunc("POST "+p+"/company/deliveries/{deliveryID}/cancel", s.auth(s.handleCompanyDeliveryCancel))

	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Fail(w, r, httpx.NotFound("no route for %s %s", r.Method, r.URL.Path))
	})
}

// ---------------------------------------------------------------------------
// middleware
// ---------------------------------------------------------------------------

type principalKey struct{}

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

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		raw, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || raw == "" {
			httpx.Fail(w, r, httpx.Unauthorized("missing bearer token"))
			return
		}
		claims, err := auth.ParseAccess(s.cfg.JWTSecret, raw)
		if err != nil {
			httpx.Fail(w, r, httpx.Unauthorized("invalid or expired access token"))
			return
		}

		// One round trip: is the session still alive, and who is the user now?
		var row principalRow
		err = s.db.WithContext(r.Context()).
			Table("sessions").
			Select("users.id, users.name, users.email, users.app_role, sessions.id AS session_id").
			Joins("JOIN users ON users.id = sessions.user_id").
			Where("sessions.id = ? AND sessions.user_id = ?", claims.SessionID, claims.UserID).
			Where("sessions.revoked_at IS NULL AND sessions.expires_at > now() AND users.deleted_at IS NULL").
			Take(&row).Error
		if err != nil {
			httpx.Fail(w, r, httpx.Unauthorized("session is no longer valid"))
			return
		}

		p := &Principal{
			UserID:    row.ID,
			Name:      row.Name,
			Email:     row.Email,
			Role:      row.AppRole,
			SessionID: row.SessionID,
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

func caller(r *http.Request) *Principal {
	p, _ := r.Context().Value(principalKey{}).(*Principal)
	return p
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "rec", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				httpx.Fail(w, r, httpx.Internal(errors.New("panic")))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *Server) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			slog.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"dur_ms", time.Since(start).Milliseconds())
		}
	})
}

// cors keeps the frontend team unblocked during development.
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return httpx.BadRequest("invalid JSON body: %v", err)
	}
	return nil
}

type paging struct {
	Limit  int
	Offset int
}

func (s *Server) paging(r *http.Request) paging {
	q := r.URL.Query()
	limit := 20
	if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > s.cfg.MaxPageSize {
		limit = s.cfg.MaxPageSize
	}
	offset := 0
	if v, err := strconv.Atoi(q.Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return paging{Limit: limit, Offset: offset}
}

func queryParam(r *http.Request, key string) string {
	return strings.TrimSpace(r.URL.Query().Get(key))
}

func boolParam(r *http.Request, key string) bool {
	switch strings.ToLower(queryParam(r, key)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// dateParam parses YYYY-MM-DD. An empty string yields nil.
func dateParam(r *http.Request, key string) (*time.Time, error) {
	raw := queryParam(r, key)
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
