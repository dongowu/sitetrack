package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/database"
	"github.com/dongowu/sitetrack/internal/httpx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

func (s *Server) handleCreateProject(c *gin.Context) {
	p := caller(c)
	var req createProjectRequest
	if err := decodeJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httpx.Fail(c, httpx.BadRequest("name is required"))
		return
	}
	start, err := parseDate(req.PlannedStart)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("planned_start must be a date in YYYY-MM-DD form"))
		return
	}
	end, err := parseDate(req.PlannedEnd)
	if err != nil {
		httpx.Fail(c, httpx.BadRequest("planned_end must be a date in YYYY-MM-DD form"))
		return
	}
	if end.Before(start) {
		httpx.Fail(c, httpx.BadRequest("planned_end cannot be before planned_start"))
		return
	}

	ctx := c.Request.Context()
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
		// This is the only statement in the API left as text: calling a sequence is
		// not a builder operation, and swapping the sequence for a counter row would
		// mean reimplementing one inside a transaction for no gain.
		if err := tx.Raw(`SELECT nextval('project_seq')`).Row().Scan(&project.Seq); err != nil {
			return err
		}
		project.Code = fmt.Sprintf("PRJ-%04d", project.Seq)
		if err := tx.Create(&project).Error; err != nil {
			return err
		}
		storeID := uuid.New()
		if err := tx.Create(&database.Store{
			ID: storeID, Kind: database.StoreKindProject, ProjectID: &project.ID, CreatedAt: time.Now().UTC(),
		}).Error; err != nil {
			return err
		}
		return tx.Create(&database.ProjectMember{
			ProjectID: project.ID, UserID: p.UserID, IsAdmin: true, JoinedAt: time.Now().UTC(),
		}).Error
	})
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	httpx.Created(c, projectDTO{
		ID: project.Code, Name: project.Name,
		PlannedStart: dateStr(project.PlannedStart), PlannedEnd: dateStr(project.PlannedEnd),
		IsProjectAdmin: true, MemberCount: 1, CreatedAt: project.CreatedAt,
	})
}

type projectListRow struct {
	ID           string    `gorm:"column:id"`
	UID          uuid.UUID `gorm:"column:uid"`
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

// visibleProjects is the row set every project screen starts from: the projects
// the caller may see, plus whether they administer each one. A superadmin needs
// no membership filter; for everyone else the join itself is the filter.
func (s *Server) visibleProjects(db *gorm.DB, p *Principal) *gorm.DB {
	q := db.Model(&database.Project{}).
		Select(`projects.id, projects.code, projects.name, projects.planned_start,
		        projects.planned_end, projects.created_at,
		        COALESCE(project_members.is_admin, false) AS is_admin`).
		Joins("LEFT JOIN project_members ON project_members.project_id = projects.id AND project_members.user_id = ?",
			p.UserID)
	if !p.IsSuperAdmin() {
		q = q.Where("project_members.user_id IS NOT NULL")
	}
	return q
}

// visibleProjectIDs is the same set narrowed to the primary key, which is what
// the roll ups below filter on.
func (s *Server) visibleProjectIDs(db *gorm.DB, p *Principal) *gorm.DB {
	return s.visibleProjects(db, p).Select("projects.id")
}

// taskRollupExpr counts a project's tasks by state in one pass. FILTER is a
// Postgres aggregate modifier with no builder equivalent, so it travels as an
// expression while the grouping and the scope stay builder calls. Late is
// derived exactly as the brief defines it: not done, and either past its planned
// end or not started past its planned start.
const taskRollupExpr = `count(*) AS total,
	count(*) FILTER (WHERE status = 'not_started') AS not_started,
	count(*) FILTER (WHERE status = 'in_progress') AS in_progress,
	count(*) FILTER (WHERE status = 'done') AS done,
	count(*) FILTER (WHERE status <> 'done'
		AND (planned_end < current_date
		     OR (status = 'not_started' AND planned_start < current_date))) AS late`

// taskRollup groups taskRollupExpr per project, restricted to the visible set so
// the aggregate never touches a project the caller cannot see.
func (s *Server) taskRollup(db *gorm.DB, p *Principal) *gorm.DB {
	return db.Model(&database.Task{}).
		Select("project_id, "+taskRollupExpr).
		Where("project_id IN (?)", s.visibleProjectIDs(db, p)).
		Group("project_id")
}

// handleListProjects answers with one statement: the page, the per project task
// roll up, the member count and the total row count come back together, because
// the alternative is one query per project on the screen.
func (s *Server) handleListProjects(c *gin.Context) {
	p := caller(c)
	pg := s.paging(c)

	search := ""
	if q := queryParam(c, "q"); q != "" {
		search = likePattern(q)
	}
	hasLate := boolParam(c, "has_late_tasks") || boolParam(c, "has_late")

	orderBy := map[string]string{
		"id":           "v.code",
		"name":         "lower(v.name)",
		"start_date":   "v.planned_start",
		"percent_done": "(CASE WHEN COALESCE(ts.total, 0) = 0 THEN 0 ELSE ts.done::float8 / ts.total END)",
		"late_tasks":   "COALESCE(ts.late, 0)",
		"created_at":   "v.created_at",
	}
	expr, ok := orderBy[queryParam(c, "sort")]
	if !ok {
		expr = orderBy["created_at"]
	}

	ctx := c.Request.Context()
	db := s.db.WithContext(ctx)

	query := db.
		Table("(?) AS v", s.visibleProjects(db, p)).
		Joins("LEFT JOIN (?) AS ts ON ts.project_id = v.id", s.taskRollup(db, p)).
		Joins("LEFT JOIN (?) AS ms ON ms.project_id = v.id",
			db.Model(&database.ProjectMember{}).
				Select("project_id, count(*) AS member_count").
				Where("project_id IN (?)", s.visibleProjectIDs(db, p)).
				Group("project_id")).
		Select(`v.code AS id, v.id AS uid, v.name,
		        to_char(v.planned_start, 'YYYY-MM-DD') AS planned_start,
		        to_char(v.planned_end, 'YYYY-MM-DD') AS planned_end,
		        v.created_at, v.is_admin,
		        COALESCE(ms.member_count, 0) AS member_count,
		        COALESCE(ts.total, 0) AS total,
		        COALESCE(ts.not_started, 0) AS not_started,
		        COALESCE(ts.in_progress, 0) AS in_progress,
		        COALESCE(ts.done, 0) AS done,
		        COALESCE(ts.late, 0) AS late,
		        count(*) OVER () AS total_rows`)

	if search != "" {
		query = query.Where("v.code ILIKE ? OR lower(v.name) LIKE ?", search, search)
	}
	if hasLate {
		query = query.Where("COALESCE(ts.late, 0) > 0")
	}

	var rows []projectListRow
	if err := query.
		Clauses(clause.OrderBy{Expression: clause.Expr{
			SQL:                expr + " " + direction(queryParam(c, "dir")) + ", v.code ASC",
			WithoutParentheses: true,
		}}).
		Limit(pg.Limit).Offset(pg.Offset).
		Scan(&rows).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	items := make([]projectDTO, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
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
		ids = append(ids, row.UID)
	}

	// Blocked counts are only needed for the rows on this page, so they are not
	// part of the big aggregate above.
	if len(ids) > 0 {
		blocked, err := s.blockedTaskCounts(ctx, ids)
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		for i := range items {
			items[i].BlockedTaskCount = blocked[ids[i]]
		}
	}

	total := 0
	if len(rows) > 0 {
		total = rows[0].TotalRows
	}
	httpx.List(c, items, map[string]any{
		"total":                total,
		"limit":                pg.Limit,
		"offset":               pg.Offset,
		"unread_notifications": s.unreadCount(ctx, p.UserID),
	})
}

// blockedTaskCounts answers "how many tasks are waiting on something that is not
// done" for a set of projects. A task counts once however many unfinished
// ancestors it has, hence the distinct; the two joins simply drop pairs where
// either end is already finished.
func (s *Server) blockedTaskCounts(ctx context.Context, projectIDs []uuid.UUID) (map[uuid.UUID]int, error) {
	var rows []struct {
		ProjectID uuid.UUID `gorm:"column:project_id"`
		Blocked   int       `gorm:"column:blocked"`
	}
	err := s.db.WithContext(ctx).
		Model(&database.TaskClosure{}).
		Select("task_closure.project_id, count(DISTINCT task_closure.descendant_id) AS blocked").
		Joins("JOIN tasks AS ancestor ON ancestor.id = task_closure.ancestor_id AND ancestor.status <> ?", statusDone).
		Joins("JOIN tasks AS descendant ON descendant.id = task_closure.descendant_id AND descendant.status <> ?", statusDone).
		Where("task_closure.project_id IN ?", projectIDs).
		Group("task_closure.project_id").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make(map[uuid.UUID]int, len(rows))
	for _, row := range rows {
		out[row.ProjectID] = row.Blocked
	}
	return out, nil
}

type projectDetailRow struct {
	Code         string    `gorm:"column:code"`
	Name         string    `gorm:"column:name"`
	PlannedStart string    `gorm:"column:planned_start"`
	PlannedEnd   string    `gorm:"column:planned_end"`
	CreatedAt    time.Time `gorm:"column:created_at"`
}

// projectTaskRollup is the shape of taskRollupExpr when it is aimed at a single
// project: one row, no grouping.
type projectTaskRollup struct {
	Total      int `gorm:"column:total"`
	NotStarted int `gorm:"column:not_started"`
	InProgress int `gorm:"column:in_progress"`
	Done       int `gorm:"column:done"`
	Late       int `gorm:"column:late"`
}

func (s *Server) handleGetProject(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ctx := c.Request.Context()
	db := s.db.WithContext(ctx)

	var row projectDetailRow
	if err := db.Model(&database.Project{}).
		Select(`projects.code, projects.name,
		        to_char(projects.planned_start, 'YYYY-MM-DD') AS planned_start,
		        to_char(projects.planned_end, 'YYYY-MM-DD') AS planned_end,
		        projects.created_at`).
		Where("projects.id = ?", a.ID).
		Scan(&row).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	// The three roll ups below are separate statements instead of seven correlated
	// subqueries. Each one is a single indexed aggregate, and the split keeps every
	// counter expressed as a GORM query rather than a string inside another query.
	var counts projectTaskRollup
	if err := db.Model(&database.Task{}).
		Select(taskRollupExpr).
		Where("project_id = ?", a.ID).
		Scan(&counts).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	var members int64
	if err := db.Model(&database.ProjectMember{}).
		Where("project_id = ?", a.ID).
		Count(&members).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	blocked, err := s.blockedTaskCounts(ctx, []uuid.UUID{a.ID})
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	httpx.OK(c, projectDTO{
		ID: row.Code, Name: row.Name, PlannedStart: row.PlannedStart, PlannedEnd: row.PlannedEnd,
		IsProjectAdmin: a.IsAdmin, MemberCount: int(members),
		TaskCounts: taskCounts{Total: counts.Total, NotStarted: counts.NotStarted,
			InProgress: counts.InProgress, Done: counts.Done},
		PercentDone:      percentDone(counts.Done, counts.Total),
		LateTaskCount:    counts.Late,
		BlockedTaskCount: blocked[a.ID],
		CreatedAt:        row.CreatedAt,
	})
}

type updateProjectRequest struct {
	Name         *string `json:"name"`
	PlannedStart *string `json:"planned_start"`
	PlannedEnd   *string `json:"planned_end"`
}

func (s *Server) handleUpdateProject(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), true)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var req updateProjectRequest
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
	if req.PlannedStart != nil {
		d, err := parseDate(*req.PlannedStart)
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("planned_start must be a date in YYYY-MM-DD form"))
			return
		}
		updates["planned_start"] = d
	}
	if req.PlannedEnd != nil {
		d, err := parseDate(*req.PlannedEnd)
		if err != nil {
			httpx.Fail(c, httpx.BadRequest("planned_end must be a date in YYYY-MM-DD form"))
			return
		}
		updates["planned_end"] = d
	}
	if len(updates) == 0 {
		httpx.Fail(c, httpx.BadRequest("nothing to update"))
		return
	}

	if err := s.db.WithContext(c.Request.Context()).Model(&database.Project{}).
		Where("id = ?", a.ID).Updates(updates).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	s.handleGetProject(c)
}

func (s *Server) handleDeleteProject(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), true)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	// Cascades to members, invitations, tasks, dependencies, closure, store,
	// stock and deliveries. The PRJ code is never handed out again because
	// project_seq is not rewound.
	if err := s.db.WithContext(c.Request.Context()).
		Where("id = ?", a.ID).Delete(&database.Project{}).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}
	httpx.NoContent(c)
}

func parseDate(v string) (time.Time, error) {
	return time.Parse(time.DateOnly, strings.TrimSpace(v))
}

var errProjectGone = errors.New("project no longer exists")
