package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/database"
	"github.com/dongowu/sitetrack/internal/httpx"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type projectAccess struct {
	ID           uuid.UUID
	Code         string
	Name         string
	PlannedStart time.Time
	PlannedEnd   time.Time
	CreatedAt    time.Time
	StoreID      uuid.UUID
	IsMember     bool
	IsAdmin      bool
}

type projectAccessRow struct {
	ID           uuid.UUID
	Code         string
	Name         string
	PlannedStart time.Time
	PlannedEnd   time.Time
	CreatedAt    time.Time
	StoreID      *uuid.UUID
	IsMember     bool
	IsAdmin      bool
}

// projectAccess resolves a PRJ code to a project and checks membership in one
// query. Anything the caller may not see is reported as not found, never as
// forbidden, so a guessed code is indistinguishable from a missing one.
func (s *Server) projectAccess(ctx context.Context, p *Principal, code string, needAdmin bool) (*projectAccess, error) {
	var row projectAccessRow
	err := s.db.WithContext(ctx).
		Table("projects AS p").
		Select(`p.id, p.code, p.name, p.planned_start, p.planned_end, p.created_at,
		        st.id AS store_id,
		        (m.user_id IS NOT NULL) AS is_member,
		        COALESCE(m.is_admin, false) AS is_admin`).
		Joins("LEFT JOIN project_members m ON m.project_id = p.id AND m.user_id = ?", p.UserID).
		Joins("LEFT JOIN stores st ON st.project_id = p.id").
		Where("p.code = upper(?)", strings.TrimSpace(code)).
		Take(&row).Error
	if isNotFound(err) {
		return nil, httpx.NotFound("project not found")
	}
	if err != nil {
		return nil, err
	}

	a := &projectAccess{
		ID: row.ID, Code: row.Code, Name: row.Name,
		PlannedStart: row.PlannedStart, PlannedEnd: row.PlannedEnd,
		CreatedAt: row.CreatedAt, IsMember: row.IsMember, IsAdmin: row.IsAdmin,
	}
	if row.StoreID != nil {
		a.StoreID = *row.StoreID
	}

	// SuperAdmins reach every project whether or not they are a member, and for
	// practical purposes they hold project admin rights.
	if p.IsSuperAdmin() {
		a.IsAdmin = true
		return a, nil
	}
	if !a.IsMember {
		return nil, httpx.NotFound("project not found")
	}
	if needAdmin && !a.IsAdmin {
		return nil, httpx.Forbidden("only a project admin can do that")
	}
	return a, nil
}

func dateStr(t time.Time) string { return t.Format(time.DateOnly) }

type taskCounts struct {
	Total      int `json:"total"`
	NotStarted int `json:"not_started"`
	InProgress int `json:"in_progress"`
	Done       int `json:"done"`
}

type projectDTO struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	PlannedStart     string     `json:"planned_start"`
	PlannedEnd       string     `json:"planned_end"`
	IsProjectAdmin   bool       `json:"is_project_admin"`
	MemberCount      int        `json:"member_count"`
	TaskCounts       taskCounts `json:"task_counts"`
	PercentDone      float64    `json:"percent_done"`
	LateTaskCount    int        `json:"late_task_count"`
	BlockedTaskCount int        `json:"blocked_task_count"`
	CreatedAt        time.Time  `json:"created_at"`
}

func percentDone(done, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(done) * 100 / float64(total)
}

// ---------------------------------------------------------------------------
// create / read / update / delete
// ---------------------------------------------------------------------------

type createProjectRequest struct {
	Name         string `json:"name"`
	PlannedStart string `json:"planned_start"`
	PlannedEnd   string `json:"planned_end"`
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	var req createProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpx.Fail(w, r, httpx.BadRequest("name is required"))
		return
	}
	start, err := parseDate(req.PlannedStart)
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("planned_start must be a date in YYYY-MM-DD form"))
		return
	}
	end, err := parseDate(req.PlannedEnd)
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("planned_end must be a date in YYYY-MM-DD form"))
		return
	}
	if end.Before(start) {
		httpx.Fail(w, r, httpx.BadRequest("planned_end cannot be before planned_start"))
		return
	}

	ctx := r.Context()
	project := database.Project{
		ID:           uuid.New(),
		Name:         req.Name,
		PlannedStart: start,
		PlannedEnd:   end,
		CreatedBy:    p.UserID,
		CreatedAt:    time.Now().UTC(),
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// nextval never returns the same number twice and never reuses one, so a
		// deleted project's code is gone for good even under concurrent creation.
		if err := tx.Raw(`SELECT nextval('project_seq')`).Row().Scan(&project.Seq); err != nil {
			return err
		}
		project.Code = fmt.Sprintf("PRJ-%04d", project.Seq)
		if err := tx.Create(&project).Error; err != nil {
			return err
		}
		storeID := uuid.New()
		if err := tx.Create(&database.Store{
			ID: storeID, Kind: "project", ProjectID: &project.ID, CreatedAt: time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		return tx.Create(&database.ProjectMember{
			ProjectID: project.ID, UserID: p.UserID, IsAdmin: true, JoinedAt: time.Now().UTC(),
		}).Error
	})
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.Created(w, projectDTO{
		ID: project.Code, Name: project.Name,
		PlannedStart: dateStr(project.PlannedStart), PlannedEnd: dateStr(project.PlannedEnd),
		IsProjectAdmin: true, MemberCount: 1, CreatedAt: project.CreatedAt,
	})
}

type projectListRow struct {
	ID           string    `gorm:"column:id"`
	Name         string    `gorm:"column:name"`
	PlannedStart string    `gorm:"column:planned_start"`
	PlannedEnd   string    `gorm:"column:planned_end"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	IsAdmin      bool      `gorm:"column:is_admin"`
	MemberCount  int       `gorm:"column:member_count"`
	Total        int       `gorm:"column:total"`
	NotStarted   int       `gorm:"column:not_started"`
	InProgress   int       `gorm:"column:in_progress"`
	Done         int       `gorm:"column:done"`
	Late         int       `gorm:"column:late"`
	TotalRows    int       `gorm:"column:total_rows"`
}

// handleListProjects keeps one hand-written query: paging, the per project task
// roll up and the sort order all have to happen in the database, otherwise the
// screen needs one query per project.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	pg := s.paging(r)

	search := ""
	if q := queryParam(r, "q"); q != "" {
		search = likePattern(q)
	}
	hasLate := boolParam(r, "has_late_tasks") || boolParam(r, "has_late")

	orderBy := map[string]string{
		"id":           "v.code",
		"name":         "lower(v.name)",
		"start_date":   "v.planned_start",
		"percent_done": "(CASE WHEN COALESCE(ts.total, 0) = 0 THEN 0 ELSE ts.done::float8 / ts.total END)",
		"late_tasks":   "COALESCE(ts.late, 0)",
		"created_at":   "v.created_at",
	}
	expr, ok := orderBy[queryParam(r, "sort")]
	if !ok {
		expr = orderBy["created_at"]
	}

	ctx := r.Context()
	var rows []projectListRow
	err := s.db.WithContext(ctx).Raw(`
		WITH visible AS (
			SELECT p.id, p.code, p.name, p.planned_start, p.planned_end, p.created_at,
			       COALESCE(m.is_admin, false) AS is_admin
			FROM projects p
			LEFT JOIN project_members m ON m.project_id = p.id AND m.user_id = ?
			WHERE ? = 'superadmin' OR m.user_id IS NOT NULL
		),
		task_stats AS (
			SELECT t.project_id,
			       count(*) AS total,
			       count(*) FILTER (WHERE t.status = 'not_started') AS not_started,
			       count(*) FILTER (WHERE t.status = 'in_progress') AS in_progress,
			       count(*) FILTER (WHERE t.status = 'done') AS done,
			       count(*) FILTER (
			           WHERE t.status <> 'done'
			             AND (t.planned_end < current_date
			                  OR (t.status = 'not_started' AND t.planned_start < current_date))
			       ) AS late
			FROM tasks t
			WHERE t.project_id IN (SELECT id FROM visible)
			GROUP BY t.project_id
		),
		member_stats AS (
			SELECT pm.project_id, count(*) AS member_count
			FROM project_members pm
			WHERE pm.project_id IN (SELECT id FROM visible)
			GROUP BY pm.project_id
		)
		SELECT v.code AS id, v.name,
		       to_char(v.planned_start, 'YYYY-MM-DD') AS planned_start,
		       to_char(v.planned_end, 'YYYY-MM-DD') AS planned_end,
		       v.created_at, v.is_admin,
		       COALESCE(ms.member_count, 0) AS member_count,
		       COALESCE(ts.total, 0) AS total,
		       COALESCE(ts.not_started, 0) AS not_started,
		       COALESCE(ts.in_progress, 0) AS in_progress,
		       COALESCE(ts.done, 0) AS done,
		       COALESCE(ts.late, 0) AS late,
		       count(*) OVER () AS total_rows
		FROM visible v
		LEFT JOIN task_stats ts ON ts.project_id = v.id
		LEFT JOIN member_stats ms ON ms.project_id = v.id
		WHERE (? = '' OR v.code ILIKE ? OR lower(v.name) LIKE ?)
		  AND (? = false OR COALESCE(ts.late, 0) > 0)
		ORDER BY `+expr+` `+direction(queryParam(r, "dir"))+`, v.code ASC
		LIMIT ? OFFSET ?`,
		p.UserID, p.Role, search, search, search, hasLate, pg.Limit, pg.Offset).Scan(&rows).Error
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	items := make([]projectDTO, 0, len(rows))
	codes := make([]string, 0, len(rows))
	for _, row := range rows {
		d := projectDTO{
			ID: row.ID, Name: row.Name, PlannedStart: row.PlannedStart, PlannedEnd: row.PlannedEnd,
			CreatedAt: row.CreatedAt, IsProjectAdmin: row.IsAdmin, MemberCount: row.MemberCount,
			TaskCounts: taskCounts{Total: row.Total, NotStarted: row.NotStarted,
				InProgress: row.InProgress, Done: row.Done},
			LateTaskCount: row.Late,
			PercentDone:   percentDone(row.Done, row.Total),
		}
		items = append(items, d)
		codes = append(codes, row.ID)
	}

	// Blocked counts are only needed for the rows on this page, so they are not
	// part of the big aggregate above.
	if len(codes) > 0 {
		blocked, err := s.blockedCountsByCode(ctx, codes)
		if err != nil {
			httpx.Fail(w, r, httpx.Internal(err))
			return
		}
		for i := range items {
			items[i].BlockedTaskCount = blocked[items[i].ID]
		}
	}

	total := 0
	if len(rows) > 0 {
		total = rows[0].TotalRows
	}
	httpx.List(w, items, map[string]any{
		"total":                total,
		"limit":                pg.Limit,
		"offset":               pg.Offset,
		"unread_notifications": s.unreadCount(ctx, p.UserID),
	})
}

func (s *Server) blockedCountsByCode(ctx context.Context, codes []string) (map[string]int, error) {
	var rows []struct {
		Code    string `gorm:"column:code"`
		Blocked int    `gorm:"column:blocked"`
	}
	err := s.db.WithContext(ctx).Raw(`
		SELECT p.code, count(DISTINCT c.descendant_id) AS blocked
		FROM task_closure c
		JOIN projects p ON p.id = c.project_id
		JOIN tasks a ON a.id = c.ancestor_id AND a.status <> 'done'
		JOIN tasks d ON d.id = c.descendant_id AND d.status <> 'done'
		WHERE p.code IN ?
		GROUP BY p.code`, codes).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, row := range rows {
		out[row.Code] = row.Blocked
	}
	return out, nil
}

type projectDetailRow struct {
	Code         string    `gorm:"column:code"`
	Name         string    `gorm:"column:name"`
	PlannedStart string    `gorm:"column:planned_start"`
	PlannedEnd   string    `gorm:"column:planned_end"`
	CreatedAt    time.Time `gorm:"column:created_at"`
	MemberCount  int       `gorm:"column:member_count"`
	Total        int       `gorm:"column:total"`
	NotStarted   int       `gorm:"column:not_started"`
	InProgress   int       `gorm:"column:in_progress"`
	Done         int       `gorm:"column:done"`
	Late         int       `gorm:"column:late"`
	Blocked      int       `gorm:"column:blocked"`
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var row projectDetailRow
	err = s.db.WithContext(r.Context()).Raw(`
		SELECT p.code, p.name,
		       to_char(p.planned_start, 'YYYY-MM-DD') AS planned_start,
		       to_char(p.planned_end, 'YYYY-MM-DD') AS planned_end,
		       p.created_at,
		       (SELECT count(*) FROM project_members WHERE project_id = p.id) AS member_count,
		       (SELECT count(*) FROM tasks WHERE project_id = p.id) AS total,
		       (SELECT count(*) FROM tasks WHERE project_id = p.id AND status = 'not_started') AS not_started,
		       (SELECT count(*) FROM tasks WHERE project_id = p.id AND status = 'in_progress') AS in_progress,
		       (SELECT count(*) FROM tasks WHERE project_id = p.id AND status = 'done') AS done,
		       (SELECT count(*) FROM tasks WHERE project_id = p.id AND status <> 'done'
		           AND (planned_end < current_date
		                OR (status = 'not_started' AND planned_start < current_date))) AS late,
		       (SELECT count(DISTINCT c.descendant_id) FROM task_closure c
		           JOIN tasks x ON x.id = c.ancestor_id AND x.status <> 'done'
		           JOIN tasks y ON y.id = c.descendant_id AND y.status <> 'done'
		           WHERE c.project_id = p.id) AS blocked
		FROM projects p WHERE p.id = ?`, a.ID).Scan(&row).Error
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.OK(w, projectDTO{
		ID: row.Code, Name: row.Name, PlannedStart: row.PlannedStart, PlannedEnd: row.PlannedEnd,
		IsProjectAdmin: a.IsAdmin, MemberCount: row.MemberCount,
		TaskCounts: taskCounts{Total: row.Total, NotStarted: row.NotStarted,
			InProgress: row.InProgress, Done: row.Done},
		PercentDone:      percentDone(row.Done, row.Total),
		LateTaskCount:    row.Late,
		BlockedTaskCount: row.Blocked,
		CreatedAt:        row.CreatedAt,
	})
}

type updateProjectRequest struct {
	Name         *string `json:"name"`
	PlannedStart *string `json:"planned_start"`
	PlannedEnd   *string `json:"planned_end"`
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req updateProjectRequest
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
	if req.PlannedStart != nil {
		d, err := parseDate(*req.PlannedStart)
		if err != nil {
			httpx.Fail(w, r, httpx.BadRequest("planned_start must be a date in YYYY-MM-DD form"))
			return
		}
		updates["planned_start"] = d
	}
	if req.PlannedEnd != nil {
		d, err := parseDate(*req.PlannedEnd)
		if err != nil {
			httpx.Fail(w, r, httpx.BadRequest("planned_end must be a date in YYYY-MM-DD form"))
			return
		}
		updates["planned_end"] = d
	}
	if len(updates) == 0 {
		httpx.Fail(w, r, httpx.BadRequest("nothing to update"))
		return
	}

	if err := s.db.WithContext(r.Context()).Model(&database.Project{}).
		Where("id = ?", a.ID).Updates(updates).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	s.handleGetProject(w, r)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	// Cascades to members, invitations, tasks, dependencies, closure, store,
	// stock and deliveries. The PRJ code is never handed out again because
	// project_seq is not rewound.
	if err := s.db.WithContext(r.Context()).
		Where("id = ?", a.ID).Delete(&database.Project{}).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.NoContent(w)
}

func parseDate(v string) (time.Time, error) {
	return time.Parse(time.DateOnly, strings.TrimSpace(v))
}

var errProjectGone = errors.New("project no longer exists")
