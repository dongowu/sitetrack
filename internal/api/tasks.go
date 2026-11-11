package api

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/database"
	"github.com/dongowu/sitetrack/internal/httpx"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	statusNotStarted = "not_started"
	statusInProgress = "in_progress"
	statusDone       = "done"
)

type taskDTO struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"project_id"`
	Name           string     `json:"name"`
	PlannedStart   string     `json:"planned_start"`
	PlannedEnd     string     `json:"planned_end"`
	Status         string     `json:"status"`
	IsLate         bool       `json:"is_late"`
	IsBlocked      bool       `json:"is_blocked"`
	DependsOn      []string   `json:"depends_on"`
	DependentCount int        `json:"dependent_count"`
	StartedAt      *time.Time `json:"started_at"`
	DoneAt         *time.Time `json:"done_at"`
	AllowedActions []string   `json:"allowed_actions,omitempty"`
}

// normalizeTaskCode accepts the full code (PRJ-0042-T-0137), the short form
// (T-0137) or a bare number (137) so links can be written either way.
func normalizeTaskCode(projectCode, raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	projectCode = strings.ToUpper(projectCode)
	switch {
	case strings.HasPrefix(raw, projectCode+"-"):
		return raw
	case strings.HasPrefix(raw, "T-"):
		return projectCode + "-" + raw
	default:
		if n, err := strconv.Atoi(raw); err == nil {
			return fmt.Sprintf("%s-T-%04d", projectCode, n)
		}
		return raw
	}
}

type taskListRow struct {
	ID           uuid.UUID  `gorm:"column:id"`
	Code         string     `gorm:"column:code"`
	Name         string     `gorm:"column:name"`
	PlannedStart time.Time  `gorm:"column:planned_start"`
	PlannedEnd   time.Time  `gorm:"column:planned_end"`
	Status       string     `gorm:"column:status"`
	StartedAt    *time.Time `gorm:"column:started_at"`
	DoneAt       *time.Time `gorm:"column:done_at"`
	IsLate       bool       `gorm:"column:is_late"`
	IsBlocked    bool       `gorm:"column:is_blocked"`
	Dependents   int        `gorm:"column:dependents"`
	TotalRows    int        `gorm:"column:total_rows"`
}

// taskFlagsExpr derives Late and Blocked for one task row. Both are derived
// states in the brief and are never stored, so they are computed by the query
// that reads them. Late is a date comparison; Blocked is an existence test
// against the closure table, which is what makes "waiting on something that is
// not done" a property of transitive dependencies rather than direct ones.
var taskFlagsExpr = fmt.Sprintf(`(tasks.status <> '%[1]s' AND (tasks.planned_end < current_date
			OR (tasks.status = '%[2]s' AND tasks.planned_start < current_date))) AS is_late,
		(tasks.status <> '%[1]s' AND EXISTS (
			SELECT 1 FROM task_closure
			JOIN tasks AS ancestor ON ancestor.id = task_closure.ancestor_id AND ancestor.status <> '%[1]s'
			WHERE task_closure.descendant_id = tasks.id)) AS is_blocked,
		(SELECT count(*) FROM task_deps WHERE task_deps.depends_on_id = tasks.id) AS dependents`,
	statusDone, statusNotStarted)

// handleListTasks answers with one statement: the flags, the dependent count,
// the filters and the total row count are all resolved in the database, because
// doing any of them in Go means one round trip per task on a project with 3,000.
func (s *Server) handleListTasks(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	pg := s.paging(c)

	status := queryParam(c, "status")
	if status != "" && status != statusNotStarted && status != statusInProgress && status != statusDone {
		httpx.Fail(c, httpx.BadRequest("status must be not_started, in_progress or done"))
		return
	}
	from, err := dateParam(c, "from")
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	to, err := dateParam(c, "to")
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	search := ""
	if q := queryParam(c, "q"); q != "" {
		search = likePattern(q)
	}

	orderBy := map[string]string{
		"id":         "seq",
		"name":       "lower(name)",
		"start_date": "planned_start",
		"end_date":   "planned_end",
		"dependents": "dependents",
	}
	expr, ok := orderBy[queryParam(c, "sort")]
	if !ok {
		expr = "seq"
	}

	ctx := c.Request.Context()
	db := s.db.WithContext(ctx)

	base := db.Model(&database.Task{}).
		Select(`tasks.id, tasks.code, tasks.name, tasks.seq, tasks.planned_start,
		        tasks.planned_end, tasks.status, tasks.started_at, tasks.done_at, `+taskFlagsExpr).
		Where("tasks.project_id = ?", a.ID)

	query := db.Table("(?) AS base", base).
		Select(`base.id, base.code, base.name, base.planned_start, base.planned_end,
		        base.status, base.started_at, base.done_at, base.is_late, base.is_blocked,
		        base.dependents, count(*) OVER () AS total_rows`)

	if status != "" {
		query = query.Where("base.status = ?", status)
	}
	if boolParam(c, "late") {
		query = query.Where("base.is_late")
	}
	if boolParam(c, "blocked") {
		query = query.Where("base.is_blocked")
	}
	if from != nil {
		query = query.Where("base.planned_end >= ?::date", *from)
	}
	if to != nil {
		query = query.Where("base.planned_start <= ?::date", *to)
	}
	if search != "" {
		query = query.Where("base.code ILIKE ? OR lower(base.name) LIKE ?", search, search)
	}

	var rows []taskListRow
	if err := query.
		Clauses(clause.OrderBy{Expression: clause.Expr{
			SQL:                expr + " " + direction(queryParam(c, "dir")) + ", seq ASC",
			WithoutParentheses: true,
		}}).
		Limit(pg.Limit).Offset(pg.Offset).
		Scan(&rows).Error; err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	items := make([]taskDTO, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		d := taskDTO{
			ID: row.Code, ProjectID: a.Code, Name: row.Name,
			PlannedStart: dateStr(row.PlannedStart), PlannedEnd: dateStr(row.PlannedEnd),
			Status: row.Status, StartedAt: row.StartedAt, DoneAt: row.DoneAt,
			IsLate: row.IsLate, IsBlocked: row.IsBlocked, DependentCount: row.Dependents,
			DependsOn: []string{},
		}
		items = append(items, d)
		ids = append(ids, row.ID)
	}

	if len(ids) > 0 {
		direct, err := s.directDependencies(ctx, ids)
		if err != nil {
			httpx.Fail(c, httpx.Internal(err))
			return
		}
		for i := range items {
			if codes, ok := direct[items[i].ID]; ok {
				items[i].DependsOn = codes
			}
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

// directDependencies returns the codes of the tasks each of the given tasks
// waits on, keyed by task code.
func (s *Server) directDependencies(ctx context.Context, ids []uuid.UUID) (map[string][]string, error) {
	var rows []struct {
		TaskID      uuid.UUID `gorm:"column:task_id"`
		TaskCode    string    `gorm:"column:task_code"`
		DependsCode string    `gorm:"column:depends_code"`
	}
	err := s.db.WithContext(ctx).
		Model(&database.TaskDep{}).
		Select(`task_deps.task_id, task.code AS task_code, dependency.code AS depends_code`).
		Joins("JOIN tasks AS task ON task.id = task_deps.task_id").
		Joins("JOIN tasks AS dependency ON dependency.id = task_deps.depends_on_id").
		Where("task_deps.task_id IN ?", ids).
		Clauses(ordering("dependency.code", "ASC")).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, row := range rows {
		out[row.TaskCode] = append(out[row.TaskCode], row.DependsCode)
	}
	return out, nil
}

type createTaskRequest struct {
	Name         string `json:"name"`
	PlannedStart string `json:"planned_start"`
	PlannedEnd   string `json:"planned_end"`
}

func (s *Server) handleCreateTask(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	var req createTaskRequest
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
	var task database.Task
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A single UPDATE ... RETURNING hands out each number exactly once; the
		// row lock is held until commit, so concurrent creators queue instead of
		// colliding. A rolled back transaction also rolls the counter back, which
		// is why no number is ever burned without producing a task.
		var project database.Project
		if err := tx.Model(&project).
			Clauses(clause.Returning{Columns: []clause.Column{{Name: "task_seq"}}}).
			Where("id = ?", a.ID).
			UpdateColumn("task_seq", gorm.Expr("task_seq + 1")).Error; err != nil {
			return err
		}
		seq := project.TaskSeq
		if seq == 0 {
			return httpx.Internal(errors.New("task sequence was not handed out"))
		}
		task = database.Task{
			ID:           uuid.New(),
			ProjectID:    a.ID,
			Seq:          seq,
			Code:         fmt.Sprintf("%s-T-%04d", a.Code, seq),
			Name:         req.Name,
			PlannedStart: start,
			PlannedEnd:   end,
			Status:       statusNotStarted,
			CreatedAt:    time.Now().UTC(),
		}
		return tx.Create(&task).Error
	})
	if err != nil {
		httpx.Fail(c, httpx.Internal(err))
		return
	}

	httpx.Created(c, taskDTO{
		ID: task.Code, ProjectID: a.Code, Name: task.Name,
		PlannedStart: dateStr(task.PlannedStart), PlannedEnd: dateStr(task.PlannedEnd),
		Status: task.Status, DependsOn: []string{}, AllowedActions: []string{"start"},
	})
}

func (s *Server) handleGetTask(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	code := normalizeTaskCode(a.Code, c.Param("taskCode"))

	d, detail, err := s.loadTaskDetail(c.Request.Context(), a, code)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, map[string]any{
		"task":     d,
		"waits_on": detail.Upstream,
		"blocks":   detail.Downstream,
	})
}

type graphNode struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	PlannedStart string `json:"planned_start"`
	PlannedEnd   string `json:"planned_end"`
	Steps        int    `json:"steps"`
}

type taskDetail struct {
	Upstream   []graphNode
	Downstream []graphNode
}

// loadTaskDetail returns the task plus every task it waits on and every task
// that waits on it, with the shortest number of steps in between.
func (s *Server) loadTaskDetail(ctx context.Context, a *projectAccess, code string) (taskDTO, taskDetail, error) {
	db := s.db.WithContext(ctx)

	var task database.Task
	err := db.Where("project_id = ? AND code = ?", a.ID, code).Take(&task).Error
	if isNotFound(err) {
		return taskDTO{}, taskDetail{}, httpx.NotFound("task not found")
	}
	if err != nil {
		return taskDTO{}, taskDetail{}, err
	}

	// The three counters that decide the allowed actions. Each is one indexed
	// aggregate; keeping them separate is what lets them stay GORM queries instead
	// of subqueries embedded in a string.
	var dependents, openAncestors, startedDescendants int64
	if err := db.Model(&database.TaskDep{}).
		Where("depends_on_id = ?", task.ID).Count(&dependents).Error; err != nil {
		return taskDTO{}, taskDetail{}, err
	}
	if err := db.Model(&database.TaskClosure{}).
		Joins("JOIN tasks AS ancestor ON ancestor.id = task_closure.ancestor_id AND ancestor.status <> ?", statusDone).
		Where("task_closure.descendant_id = ?", task.ID).
		Count(&openAncestors).Error; err != nil {
		return taskDTO{}, taskDetail{}, err
	}
	if err := db.Model(&database.TaskClosure{}).
		Joins("JOIN tasks AS descendant ON descendant.id = task_closure.descendant_id AND descendant.status <> ?", statusNotStarted).
		Where("task_closure.ancestor_id = ?", task.ID).
		Count(&startedDescendants).Error; err != nil {
		return taskDTO{}, taskDetail{}, err
	}

	deps := []string{}
	if err := db.Table("task_deps AS d").
		Select("t.code").
		Joins("JOIN tasks t ON t.id = d.depends_on_id").
		Where("d.task_id = ?", task.ID).
		Order("t.code").Scan(&deps).Error; err != nil {
		return taskDTO{}, taskDetail{}, err
	}

	d := taskDTO{
		ID: task.Code, ProjectID: a.Code, Name: task.Name,
		PlannedStart: dateStr(task.PlannedStart), PlannedEnd: dateStr(task.PlannedEnd),
		Status: task.Status, StartedAt: task.StartedAt, DoneAt: task.DoneAt,
		DependsOn: deps, DependentCount: int(dependents),
		IsLate:    isLate(task.Status, task.PlannedStart, task.PlannedEnd, time.Now()),
		IsBlocked: task.Status != statusDone && openAncestors > 0,
	}
	d.AllowedActions = allowedActions(task.Status, int(openAncestors), int(startedDescendants))

	detail := taskDetail{}
	if detail.Upstream, err = s.loadGraph(ctx, task.ID, true); err != nil {
		return d, detail, err
	}
	if detail.Downstream, err = s.loadGraph(ctx, task.ID, false); err != nil {
		return d, detail, err
	}
	return d, detail, nil
}

func (s *Server) loadGraph(ctx context.Context, taskID uuid.UUID, upstream bool) ([]graphNode, error) {
	const projection = `tasks.code AS id, tasks.name, tasks.status, tasks.planned_start,
		tasks.planned_end, task_closure.depth AS steps`

	db := s.db.WithContext(ctx)
	query := db.Model(&database.Task{}).
		Select(projection).
		Joins("JOIN task_closure ON task_closure.ancestor_id = tasks.id").
		Where("task_closure.descendant_id = ?", taskID)
	if !upstream {
		query = db.Model(&database.Task{}).
			Select(projection).
			Joins("JOIN task_closure ON task_closure.descendant_id = tasks.id").
			Where("task_closure.ancestor_id = ?", taskID)
	}

	var rows []struct {
		ID     string    `gorm:"column:id"`
		Name   string    `gorm:"column:name"`
		Status string    `gorm:"column:status"`
		Start  time.Time `gorm:"column:planned_start"`
		End    time.Time `gorm:"column:planned_end"`
		Steps  int       `gorm:"column:steps"`
	}
	if err := query.Clauses(ordering("task_closure.depth, tasks.code", "ASC")).
		Scan(&rows).Error; err != nil {
		return nil, err
	}

	out := []graphNode{}
	for _, row := range rows {
		out = append(out, graphNode{
			ID: row.ID, Name: row.Name, Status: row.Status,
			PlannedStart: dateStr(row.Start), PlannedEnd: dateStr(row.End), Steps: row.Steps,
		})
	}
	return out, nil
}

func allowedActions(status string, openAncestors, startedDescendants int) []string {
	actions := []string{}
	switch status {
	case statusNotStarted:
		if openAncestors == 0 {
			actions = append(actions, "start")
		}
	case statusInProgress:
		actions = append(actions, "done")
	case statusDone:
		if startedDescendants == 0 {
			actions = append(actions, "reopen")
		}
	}
	return actions
}

func isLate(status string, start, end time.Time, now time.Time) bool {
	if status == statusDone {
		return false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if end.Before(today) {
		return true
	}
	return status == statusNotStarted && start.Before(today)
}

type updateTaskRequest struct {
	Name         *string `json:"name"`
	PlannedStart *string `json:"planned_start"`
	PlannedEnd   *string `json:"planned_end"`
}

func (s *Server) handleUpdateTask(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	code := normalizeTaskCode(a.Code, c.Param("taskCode"))

	var req updateTaskRequest
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

	result := s.db.WithContext(c.Request.Context()).Model(&database.Task{}).
		Where("project_id = ? AND code = ?", a.ID, code).Updates(updates)
	if result.Error != nil {
		httpx.Fail(c, httpx.Internal(result.Error))
		return
	}
	if result.RowsAffected == 0 {
		httpx.Fail(c, httpx.NotFound("task not found"))
		return
	}
	s.handleGetTask(c)
}

func (s *Server) handleDeleteTask(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	code := normalizeTaskCode(a.Code, c.Param("taskCode"))

	err = s.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := lockProject(tx, a.ID); err != nil {
			return err
		}
		result := tx.Where("project_id = ? AND code = ?", a.ID, code).Delete(&database.Task{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return httpx.NotFound("task not found")
		}
		// Removing a node invalidates every path that ran through it, so the
		// closure is rebuilt for this project only.
		return rebuildClosure(tx, a.ID)
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.NoContent(c)
}

// ---------------------------------------------------------------------------
// status transitions
// ---------------------------------------------------------------------------

func (s *Server) transition(c *gin.Context, action string) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	code := normalizeTaskCode(a.Code, c.Param("taskCode"))

	err = s.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		var task database.Task
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("project_id = ? AND code = ?", a.ID, code).Take(&task).Error
		if isNotFound(err) {
			return httpx.NotFound("task not found")
		}
		if err != nil {
			return err
		}

		now := time.Now().UTC()
		switch action {
		case "start":
			if task.Status != statusNotStarted {
				return httpx.Unprocessable("only a task that has not started can be started")
			}
			var openAncestors int64
			if err := tx.Model(&database.TaskClosure{}).
				Joins("JOIN tasks AS ancestor ON ancestor.id = task_closure.ancestor_id AND ancestor.status <> ?", statusDone).
				Where("task_closure.descendant_id = ?", task.ID).
				Count(&openAncestors).Error; err != nil {
				return err
			}
			if openAncestors > 0 {
				return httpx.Unprocessable("this task still waits on %d task(s) that are not done", openAncestors)
			}
			return tx.Model(&database.Task{}).Where("id = ?", task.ID).
				Updates(map[string]any{"status": statusInProgress, "started_at": now}).Error

		case "done":
			if task.Status != statusInProgress {
				return httpx.Unprocessable("only a task in progress can be marked as done")
			}
			return tx.Model(&database.Task{}).Where("id = ?", task.ID).
				Updates(map[string]any{"status": statusDone, "done_at": now}).Error

		case "reopen":
			if task.Status != statusDone {
				return httpx.Unprocessable("only a done task can be reopened")
			}
			var startedDescendants int64
			if err := tx.Model(&database.TaskClosure{}).
				Joins("JOIN tasks AS descendant ON descendant.id = task_closure.descendant_id AND descendant.status <> ?", statusNotStarted).
				Where("task_closure.ancestor_id = ?", task.ID).
				Count(&startedDescendants).Error; err != nil {
				return err
			}
			if startedDescendants > 0 {
				return httpx.Unprocessable("%d task(s) that wait on this one have already started", startedDescendants)
			}
			return tx.Model(&database.Task{}).Where("id = ?", task.ID).
				Updates(map[string]any{"status": statusInProgress, "done_at": nil}).Error
		}
		return httpx.BadRequest("unknown action")
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	s.handleGetTask(c)
}

func (s *Server) handleTaskStart(c *gin.Context) {
	s.transition(c, "start")
}

func (s *Server) handleTaskDone(c *gin.Context) {
	s.transition(c, "done")
}

func (s *Server) handleTaskReopen(c *gin.Context) {
	s.transition(c, "reopen")
}

// ---------------------------------------------------------------------------
// dependencies
// ---------------------------------------------------------------------------

type addDependencyRequest struct {
	DependsOn string `json:"depends_on"`
}

func (s *Server) handleAddDependency(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	code := normalizeTaskCode(a.Code, c.Param("taskCode"))

	var req addDependencyRequest
	if err := decodeJSON(c, &req); err != nil {
		httpx.Fail(c, err)
		return
	}
	if strings.TrimSpace(req.DependsOn) == "" {
		httpx.Fail(c, httpx.BadRequest("depends_on is required"))
		return
	}
	dependsOnCode := normalizeTaskCode(a.Code, req.DependsOn)
	if code == dependsOnCode {
		httpx.Fail(c, httpx.Unprocessable("a task cannot depend on itself"))
		return
	}

	err = s.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		// One writer at a time per project: the cycle check and the closure insert
		// must see each other, otherwise two concurrent edges can close a loop.
		if err := lockProject(tx, a.ID); err != nil {
			return err
		}

		var tasks []database.Task
		if err := tx.Where("project_id = ? AND code IN ?", a.ID, []string{code, dependsOnCode}).
			Find(&tasks).Error; err != nil {
			return err
		}
		var taskID, dependsOnID uuid.UUID
		for _, t := range tasks {
			if t.Code == code {
				taskID = t.ID
			}
			if t.Code == dependsOnCode {
				dependsOnID = t.ID
			}
		}
		if taskID == uuid.Nil {
			return httpx.NotFound("task %s not found", code)
		}
		if dependsOnID == uuid.Nil {
			return httpx.NotFound("task %s not found", dependsOnCode)
		}
		if taskID == dependsOnID {
			return httpx.Unprocessable("a task cannot depend on itself")
		}

		// Cycle: does the prerequisite already wait on this task?
		var cyclePath int64
		if err := tx.Model(&database.TaskClosure{}).
			Where("ancestor_id = ? AND descendant_id = ?", taskID, dependsOnID).
			Count(&cyclePath).Error; err != nil {
			return err
		}
		if cyclePath > 0 {
			return httpx.Unprocessable(
				"%s already waits on %s, so this dependency would create a cycle", dependsOnCode, code)
		}

		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&database.TaskDep{
			ProjectID: a.ID, TaskID: taskID, DependsOnID: dependsOnID, CreatedAt: time.Now().UTC(),
		}).Error; err != nil {
			return err
		}

		// Close the transitive closure: every ancestor of the prerequisite (itself
		// included) now reaches every descendant of this task (itself included).
		return tx.Exec(`
			INSERT INTO task_closure (project_id, ancestor_id, descendant_id, depth)
			SELECT ?, anc.ancestor_id, des.descendant_id, anc.depth + 1 + des.depth
			FROM (
				SELECT ?::uuid AS ancestor_id, 0 AS depth
				UNION ALL
				SELECT ancestor_id, depth FROM task_closure WHERE descendant_id = ?
			) anc
			CROSS JOIN (
				SELECT ?::uuid AS descendant_id, 0 AS depth
				UNION ALL
				SELECT descendant_id, depth FROM task_closure WHERE ancestor_id = ?
			) des
			ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`,
			a.ID, dependsOnID, dependsOnID, taskID, taskID).Error
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	s.handleGetTask(c)
}

func (s *Server) handleRemoveDependency(c *gin.Context) {
	p := caller(c)
	a, err := s.projectAccess(c.Request.Context(), p, c.Param("projectCode"), false)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	code := normalizeTaskCode(a.Code, c.Param("taskCode"))
	depCode := normalizeTaskCode(a.Code, c.Param("dependsOnCode"))

	err = s.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := lockProject(tx, a.ID); err != nil {
			return err
		}
		var tasks []database.Task
		if err := tx.Where("project_id = ? AND code IN ?", a.ID, []string{code, depCode}).
			Find(&tasks).Error; err != nil {
			return err
		}
		var taskID, dependsOnID uuid.UUID
		for _, t := range tasks {
			switch t.Code {
			case code:
				taskID = t.ID
			case depCode:
				dependsOnID = t.ID
			}
		}
		if taskID == uuid.Nil || dependsOnID == uuid.Nil {
			return httpx.NotFound("no such dependency")
		}

		result := tx.Where("project_id = ? AND task_id = ? AND depends_on_id = ?",
			a.ID, taskID, dependsOnID).Delete(&database.TaskDep{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return httpx.NotFound("no such dependency")
		}
		return rebuildClosure(tx, a.ID)
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	s.handleGetTask(c)
}

// handleGetTaskGraph is kept for the frontend that wants the graph on its own.
func (s *Server) handleGetTaskGraph(c *gin.Context) {
	s.handleGetTask(c)
}

// rebuildClosure recomputes the transitive closure for one project with a
// breadth first sweep, one depth level per statement. Deleting an edge can
// invalidate paths that ran through it, so the affected pairs have to be
// recomputed rather than removed. Level by level (instead of one recursive CTE)
// keeps the work polynomial even on a densely connected project.
func rebuildClosure(tx *gorm.DB, projectID uuid.UUID) error {
	if err := tx.Where("project_id = ?", projectID).Delete(&database.TaskClosure{}).Error; err != nil {
		return err
	}

	// The two statements below are the only ones left as text. GORM has no
	// builder for INSERT ... SELECT, and the alternative - one Create per pair -
	// turns a set operation into tens of thousands of round trips on a project
	// with 3,000 tasks. The surrounding delete above is a normal GORM Delete.
	result := tx.Exec(`
		INSERT INTO task_closure (project_id, ancestor_id, descendant_id, depth)
		SELECT ?, depends_on_id, task_id, 1 FROM task_deps WHERE project_id = ?
		ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`, projectID, projectID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return nil
	}

	for depth := 2; depth <= 10000; depth++ {
		result := tx.Exec(`
			INSERT INTO task_closure (project_id, ancestor_id, descendant_id, depth)
			SELECT ?, c.ancestor_id, d.task_id, ?
			FROM task_closure c
			JOIN task_deps d ON d.depends_on_id = c.descendant_id AND d.project_id = ?
			WHERE c.project_id = ? AND c.depth = ?
			ON CONFLICT (ancestor_id, descendant_id) DO NOTHING`,
			projectID, depth, projectID, projectID, depth-1)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
	}
	return nil
}

var errTaskGone = errors.New("task no longer exists")
