package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/database"
	"github.com/dongowu/sitetrack/internal/httpx"
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

// handleListTasks keeps one hand-written query: the Late and Blocked flags are
// derived per row from the closure table, and doing that in Go would mean one
// extra round trip per task on a project with 3,000 of them.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	pg := s.paging(r)

	status := queryParam(r, "status")
	if status != "" && status != statusNotStarted && status != statusInProgress && status != statusDone {
		httpx.Fail(w, r, httpx.BadRequest("status must be not_started, in_progress or done"))
		return
	}
	from, err := dateParam(r, "from")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	to, err := dateParam(r, "to")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	search := ""
	if q := queryParam(r, "q"); q != "" {
		search = likePattern(q)
	}

	orderBy := map[string]string{
		"id":         "seq",
		"name":       "lower(name)",
		"start_date": "planned_start",
		"end_date":   "planned_end",
		"dependents": "dependents",
	}
	expr, ok := orderBy[queryParam(r, "sort")]
	if !ok {
		expr = "seq"
	}

	ctx := r.Context()
	var rows []taskListRow
	err = s.db.WithContext(ctx).Raw(`
		WITH base AS (
			SELECT t.id, t.code, t.name, t.seq, t.planned_start, t.planned_end, t.status,
			       t.started_at, t.done_at,
			       (t.status <> 'done' AND (
			            t.planned_end < current_date
			            OR (t.status = 'not_started' AND t.planned_start < current_date))) AS is_late,
			       (t.status <> 'done' AND EXISTS (
			            SELECT 1 FROM task_closure c
			            JOIN tasks anc ON anc.id = c.ancestor_id AND anc.status <> 'done'
			            WHERE c.descendant_id = t.id)) AS is_blocked,
			       (SELECT count(*) FROM task_deps d WHERE d.depends_on_id = t.id) AS dependents
			FROM tasks t
			WHERE t.project_id = ?
		)
		SELECT id, code, name, planned_start, planned_end, status, started_at, done_at,
		       is_late, is_blocked, dependents,
		       count(*) OVER () AS total_rows
		FROM base
		WHERE (? = '' OR status = ?)
		  AND (? = false OR is_late)
		  AND (? = false OR is_blocked)
		  AND (?::date IS NULL OR planned_end >= ?::date)
		  AND (?::date IS NULL OR planned_start <= ?::date)
		  AND (? = '' OR code ILIKE ? OR lower(name) LIKE ?)
		ORDER BY `+expr+` `+direction(queryParam(r, "dir"))+`, seq ASC
		LIMIT ? OFFSET ?`,
		a.ID,
		status, status,
		boolParam(r, "late"),
		boolParam(r, "blocked"),
		from, from,
		to, to,
		search, search, search,
		pg.Limit, pg.Offset).Scan(&rows).Error
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
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
			httpx.Fail(w, r, httpx.Internal(err))
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
	httpx.List(w, items, map[string]any{
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
	err := s.db.WithContext(ctx).Raw(`
		SELECT d.task_id, t.code AS task_code, dep.code AS depends_code
		FROM task_deps d
		JOIN tasks t ON t.id = d.task_id
		JOIN tasks dep ON dep.id = d.depends_on_id
		WHERE d.task_id IN ?
		ORDER BY dep.code`, ids).Scan(&rows).Error
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

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req createTaskRequest
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
	var task database.Task
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A single UPDATE ... RETURNING hands out each number exactly once; the
		// row lock is held until commit, so concurrent creators queue instead of
		// colliding. A rolled back transaction also rolls the counter back, which
		// is why no number is ever burned without producing a task.
		var seq int
		if err := tx.Raw(`UPDATE projects SET task_seq = task_seq + 1 WHERE id = ? RETURNING task_seq`,
			a.ID).Row().Scan(&seq); err != nil {
			return err
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
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.Created(w, taskDTO{
		ID: task.Code, ProjectID: a.Code, Name: task.Name,
		PlannedStart: dateStr(task.PlannedStart), PlannedEnd: dateStr(task.PlannedEnd),
		Status: task.Status, DependsOn: []string{}, AllowedActions: []string{"start"},
	})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	code := normalizeTaskCode(a.Code, r.PathValue("taskCode"))

	d, detail, err := s.loadTaskDetail(r.Context(), a, code)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, map[string]any{
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

type taskDetailRow struct {
	ID                 uuid.UUID  `gorm:"column:id"`
	Code               string     `gorm:"column:code"`
	Name               string     `gorm:"column:name"`
	Start              time.Time  `gorm:"column:planned_start"`
	End                time.Time  `gorm:"column:planned_end"`
	Status             string     `gorm:"column:status"`
	StartedAt          *time.Time `gorm:"column:started_at"`
	DoneAt             *time.Time `gorm:"column:done_at"`
	Dependents         int        `gorm:"column:dependents"`
	OpenAncestors      int        `gorm:"column:open_ancestors"`
	StartedDescendants int        `gorm:"column:started_descendants"`
}

// loadTaskDetail returns the task plus every task it waits on and every task
// that waits on it, with the shortest number of steps in between.
func (s *Server) loadTaskDetail(ctx context.Context, a *projectAccess, code string) (taskDTO, taskDetail, error) {
	var row taskDetailRow
	err := s.db.WithContext(ctx).Raw(`
		SELECT t.id, t.code, t.name, t.planned_start, t.planned_end, t.status,
		       t.started_at, t.done_at,
		       (SELECT count(*) FROM task_deps d WHERE d.depends_on_id = t.id) AS dependents,
		       (SELECT count(*) FROM task_closure c
		          JOIN tasks anc ON anc.id = c.ancestor_id AND anc.status <> 'done'
		          WHERE c.descendant_id = t.id) AS open_ancestors,
		       (SELECT count(*) FROM task_closure c
		          JOIN tasks des ON des.id = c.descendant_id AND des.status <> 'not_started'
		          WHERE c.ancestor_id = t.id) AS started_descendants
		FROM tasks t
		WHERE t.project_id = ? AND t.code = ?`, a.ID, code).Scan(&row).Error
	if err != nil {
		return taskDTO{}, taskDetail{}, err
	}
	if row.ID == uuid.Nil {
		return taskDTO{}, taskDetail{}, httpx.NotFound("task not found")
	}

	deps := []string{}
	if err := s.db.WithContext(ctx).Table("task_deps AS d").
		Select("t.code").
		Joins("JOIN tasks t ON t.id = d.depends_on_id").
		Where("d.task_id = ?", row.ID).
		Order("t.code").Scan(&deps).Error; err != nil {
		return taskDTO{}, taskDetail{}, err
	}

	d := taskDTO{
		ID: row.Code, ProjectID: a.Code, Name: row.Name,
		PlannedStart: dateStr(row.Start), PlannedEnd: dateStr(row.End),
		Status: row.Status, StartedAt: row.StartedAt, DoneAt: row.DoneAt,
		DependsOn: deps, DependentCount: row.Dependents,
		IsLate:    isLate(row.Status, row.Start, row.End, time.Now()),
		IsBlocked: row.Status != statusDone && row.OpenAncestors > 0,
	}
	d.AllowedActions = allowedActions(row.Status, row.OpenAncestors, row.StartedDescendants)

	detail := taskDetail{}
	if detail.Upstream, err = s.loadGraph(ctx, row.ID, true); err != nil {
		return d, detail, err
	}
	if detail.Downstream, err = s.loadGraph(ctx, row.ID, false); err != nil {
		return d, detail, err
	}
	return d, detail, nil
}

func (s *Server) loadGraph(ctx context.Context, taskID uuid.UUID, upstream bool) ([]graphNode, error) {
	query := `
		SELECT t.code AS id, t.name, t.status, t.planned_start, t.planned_end, c.depth AS steps
		FROM task_closure c
		JOIN tasks t ON t.id = c.ancestor_id
		WHERE c.descendant_id = ?
		ORDER BY c.depth, t.code`
	if !upstream {
		query = `
		SELECT t.code AS id, t.name, t.status, t.planned_start, t.planned_end, c.depth AS steps
		FROM task_closure c
		JOIN tasks t ON t.id = c.descendant_id
		WHERE c.ancestor_id = ?
		ORDER BY c.depth, t.code`
	}

	var rows []struct {
		ID     string    `gorm:"column:id"`
		Name   string    `gorm:"column:name"`
		Status string    `gorm:"column:status"`
		Start  time.Time `gorm:"column:planned_start"`
		End    time.Time `gorm:"column:planned_end"`
		Steps  int       `gorm:"column:steps"`
	}
	if err := s.db.WithContext(ctx).Raw(query, taskID).Scan(&rows).Error; err != nil {
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

func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	code := normalizeTaskCode(a.Code, r.PathValue("taskCode"))

	var req updateTaskRequest
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

	result := s.db.WithContext(r.Context()).Model(&database.Task{}).
		Where("project_id = ? AND code = ?", a.ID, code).Updates(updates)
	if result.Error != nil {
		httpx.Fail(w, r, httpx.Internal(result.Error))
		return
	}
	if result.RowsAffected == 0 {
		httpx.Fail(w, r, httpx.NotFound("task not found"))
		return
	}
	s.handleGetTask(w, r)
}

func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	code := normalizeTaskCode(a.Code, r.PathValue("taskCode"))

	err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
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
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w)
}

// ---------------------------------------------------------------------------
// status transitions
// ---------------------------------------------------------------------------

func (s *Server) transition(w http.ResponseWriter, r *http.Request, action string) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	code := normalizeTaskCode(a.Code, r.PathValue("taskCode"))

	err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
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
			if err := tx.Raw(`
				SELECT count(*) FROM task_closure c
				JOIN tasks a ON a.id = c.ancestor_id AND a.status <> 'done'
				WHERE c.descendant_id = ?`, task.ID).Row().Scan(&openAncestors); err != nil {
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
			if err := tx.Raw(`
				SELECT count(*) FROM task_closure c
				JOIN tasks d ON d.id = c.descendant_id AND d.status <> 'not_started'
				WHERE c.ancestor_id = ?`, task.ID).Row().Scan(&startedDescendants); err != nil {
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
		httpx.Fail(w, r, err)
		return
	}
	s.handleGetTask(w, r)
}

func (s *Server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "start")
}

func (s *Server) handleTaskDone(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "done")
}

func (s *Server) handleTaskReopen(w http.ResponseWriter, r *http.Request) {
	s.transition(w, r, "reopen")
}

// ---------------------------------------------------------------------------
// dependencies
// ---------------------------------------------------------------------------

type addDependencyRequest struct {
	DependsOn string `json:"depends_on"`
}

func (s *Server) handleAddDependency(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	code := normalizeTaskCode(a.Code, r.PathValue("taskCode"))

	var req addDependencyRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if strings.TrimSpace(req.DependsOn) == "" {
		httpx.Fail(w, r, httpx.BadRequest("depends_on is required"))
		return
	}
	dependsOnCode := normalizeTaskCode(a.Code, req.DependsOn)
	if code == dependsOnCode {
		httpx.Fail(w, r, httpx.Unprocessable("a task cannot depend on itself"))
		return
	}

	err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
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
		var wouldCycle bool
		if err := tx.Raw(`SELECT EXISTS (SELECT 1 FROM task_closure WHERE ancestor_id = ? AND descendant_id = ?)`,
			taskID, dependsOnID).Row().Scan(&wouldCycle); err != nil {
			return err
		}
		if wouldCycle {
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
		httpx.Fail(w, r, err)
		return
	}
	s.handleGetTask(w, r)
}

func (s *Server) handleRemoveDependency(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	code := normalizeTaskCode(a.Code, r.PathValue("taskCode"))
	depCode := normalizeTaskCode(a.Code, r.PathValue("dependsOnCode"))

	err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := lockProject(tx, a.ID); err != nil {
			return err
		}
		result := tx.Exec(`
			DELETE FROM task_deps d
			USING tasks t, tasks dep
			WHERE d.task_id = t.id AND d.depends_on_id = dep.id
			  AND d.project_id = ? AND t.code = ? AND dep.code = ?`, a.ID, code, depCode)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return httpx.NotFound("no such dependency")
		}
		return rebuildClosure(tx, a.ID)
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.handleGetTask(w, r)
}

// handleGetTaskGraph is kept for the frontend that wants the graph on its own.
func (s *Server) handleGetTaskGraph(w http.ResponseWriter, r *http.Request) {
	s.handleGetTask(w, r)
}

// rebuildClosure recomputes the transitive closure for one project with a
// breadth first sweep, one depth level per statement. Deleting an edge can
// invalidate paths that ran through it, so the affected pairs have to be
// recomputed rather than removed. Level by level (instead of one recursive CTE)
// keeps the work polynomial even on a densely connected project.
func rebuildClosure(tx *gorm.DB, projectID uuid.UUID) error {
	if err := tx.Exec(`DELETE FROM task_closure WHERE project_id = ?`, projectID).Error; err != nil {
		return err
	}
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
