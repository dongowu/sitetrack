package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/database"
	"github.com/dongowu/sitetrack/internal/httpx"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type deliveryLineDTO struct {
	MaterialID string  `json:"material_id"`
	Name       string  `json:"name"`
	Unit       string  `json:"unit"`
	Quantity   float64 `json:"quantity"`
}

type deliveryDTO struct {
	ID             string            `json:"id"`
	ProjectID      string            `json:"project_id"`
	ProjectName    string            `json:"project_name,omitempty"`
	Direction      string            `json:"direction"`
	From           string            `json:"from"`
	To             string            `json:"to"`
	Status         string            `json:"status"`
	Lines          []deliveryLineDTO `json:"lines"`
	CreatedBy      string            `json:"created_by"`
	CreatedAt      time.Time         `json:"created_at"`
	ResolvedAt     *time.Time        `json:"resolved_at"`
	ResolvedBy     *string           `json:"resolved_by"`
	AllowedActions []string          `json:"allowed_actions"`
}

type deliveryScope struct {
	// viewerStoreID is the store the caller is looking from; it decides the
	// incoming/outgoing label and which actions are offered.
	viewerStoreID uuid.UUID
	projectID     uuid.UUID // nil for the company wide view
}

type deliveryRow struct {
	ID          uuid.UUID  `gorm:"column:id"`
	ProjectCode string     `gorm:"column:project_code"`
	ProjectName string     `gorm:"column:project_name"`
	Incoming    bool       `gorm:"column:incoming"`
	Status      string     `gorm:"column:status"`
	CreatedAt   time.Time  `gorm:"column:created_at"`
	ResolvedAt  *time.Time `gorm:"column:resolved_at"`
	CreatedBy   string     `gorm:"column:created_by"`
	ResolvedBy  *string    `gorm:"column:resolved_by"`
}

func (s *Server) handleProjectDeliveries(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.listDeliveries(w, r, deliveryScope{viewerStoreID: a.StoreID, projectID: a.ID})
}

func (s *Server) handleCompanyDeliveries(w http.ResponseWriter, r *http.Request) {
	_, storeID, ok := s.requireCompanyStore(w, r)
	if !ok {
		return
	}
	s.listDeliveries(w, r, deliveryScope{viewerStoreID: storeID})
}

func (s *Server) listDeliveries(w http.ResponseWriter, r *http.Request, scope deliveryScope) {
	pg := s.paging(r)
	ctx := r.Context()

	dir := strings.ToLower(queryParam(r, "direction"))
	switch dir {
	case "", "in", "out", "incoming", "outgoing":
	default:
		httpx.Fail(w, r, httpx.BadRequest("direction must be in, out, incoming or outgoing"))
		return
	}
	if dir == "incoming" {
		dir = "in"
	}
	if dir == "outgoing" {
		dir = "out"
	}

	status := strings.ToLower(queryParam(r, "status"))
	switch status {
	case "", "in_transit", "delivered", "rejected", "cancelled":
	default:
		httpx.Fail(w, r, httpx.BadRequest("status must be in_transit, delivered, rejected or cancelled"))
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

	base := s.db.WithContext(ctx).
		Table("deliveries AS d").
		Joins("JOIN projects p ON p.id = d.project_id").
		Where("(d.from_store_id = ? OR d.to_store_id = ?)", scope.viewerStoreID, scope.viewerStoreID)
	if scope.projectID != uuid.Nil {
		base = base.Where("d.project_id = ?", scope.projectID)
	}
	switch dir {
	case "in":
		base = base.Where("d.to_store_id = ?", scope.viewerStoreID)
	case "out":
		base = base.Where("d.from_store_id = ?", scope.viewerStoreID)
	}
	if status != "" {
		base = base.Where("d.status = ?", status)
	}
	if from != nil {
		base = base.Where("d.created_at >= ?", *from)
	}
	if to != nil {
		base = base.Where("d.created_at < ?", to.AddDate(0, 0, 1))
	}
	if scope.projectID == uuid.Nil {
		if proj := queryParam(r, "project"); proj != "" {
			base = base.Where("p.code = ?", strings.ToUpper(proj))
		}
	}

	var total int64
	if err := base.Count(&total).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	var rows []deliveryRow
	err = base.
		Select(`d.id, p.code AS project_code, p.name AS project_name,
		        (d.to_store_id = ?) AS incoming, d.status, d.created_at, d.resolved_at,
		        cu.name AS created_by, ru.name AS resolved_by`, scope.viewerStoreID).
		Joins("JOIN users cu ON cu.id = d.created_by").
		Joins("LEFT JOIN users ru ON ru.id = d.resolved_by").
		Order("d.created_at DESC, d.id DESC").
		Limit(pg.Limit).Offset(pg.Offset).Scan(&rows).Error
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	items := make([]deliveryDTO, 0, len(rows))
	ids := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		d := deliveryDTO{
			ID: row.ID.String(), ProjectID: row.ProjectCode, ProjectName: row.ProjectName,
			Status: row.Status, CreatedAt: row.CreatedAt, ResolvedAt: row.ResolvedAt,
			CreatedBy: row.CreatedBy, ResolvedBy: row.ResolvedBy,
		}
		if row.Incoming {
			d.Direction, d.From, d.To = "incoming", "company_store", "project_store"
		} else {
			d.Direction, d.From, d.To = "outgoing", "project_store", "company_store"
		}
		d.AllowedActions = deliveryActions(d.Status, row.Incoming)
		items = append(items, d)
		ids = append(ids, row.ID)
	}

	if len(ids) > 0 {
		lines, err := s.deliveryLines(ctx, ids)
		if err != nil {
			httpx.Fail(w, r, httpx.Internal(err))
			return
		}
		for i := range items {
			items[i].Lines = lines[items[i].ID]
			if items[i].Lines == nil {
				items[i].Lines = []deliveryLineDTO{}
			}
		}
	}

	p := caller(r)
	httpx.List(w, items, map[string]any{
		"total": total, "limit": pg.Limit, "offset": pg.Offset,
		"unread_notifications": s.unreadCount(ctx, p.UserID),
	})
}

func deliveryActions(status string, incoming bool) []string {
	if status != "in_transit" {
		return []string{}
	}
	if incoming {
		return []string{"record", "reject"}
	}
	return []string{"cancel"}
}

func (s *Server) deliveryLines(ctx context.Context, ids []uuid.UUID) (map[string][]deliveryLineDTO, error) {
	var rows []struct {
		DeliveryID uuid.UUID `gorm:"column:delivery_id"`
		MaterialID uuid.UUID `gorm:"column:material_id"`
		Name       string    `gorm:"column:name"`
		Unit       string    `gorm:"column:unit"`
		Quantity   float64   `gorm:"column:quantity"`
	}
	err := s.db.WithContext(ctx).
		Table("delivery_lines AS l").
		Select("l.delivery_id, m.id AS material_id, m.name, m.unit, l.quantity::float8 AS quantity").
		Joins("JOIN materials m ON m.id = l.material_id").
		Where("l.delivery_id IN ?", ids).
		Order("lower(m.name)").Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := map[string][]deliveryLineDTO{}
	for _, row := range rows {
		key := row.DeliveryID.String()
		out[key] = append(out[key], deliveryLineDTO{
			MaterialID: row.MaterialID.String(), Name: row.Name, Unit: row.Unit, Quantity: row.Quantity,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// create
// ---------------------------------------------------------------------------

type deliveryLineRequest struct {
	MaterialID string   `json:"material_id"`
	Quantity   *float64 `json:"quantity"`
}

type createDeliveryRequest struct {
	ProjectID string                `json:"project_id"`
	Lines     []deliveryLineRequest `json:"lines"`
}

// handleProjectCreateDelivery sends material from the project store back to the
// company store. Project stores never deliver to each other.
func (s *Server) handleProjectCreateDelivery(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req createDeliveryRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	_, companyStoreID, ok := s.requireCompanyStore(w, r)
	if !ok {
		// The company store row must exist, but a project member is allowed to
		// send material back to it without being a store manager.
		var company database.Store
		if err := s.db.WithContext(r.Context()).Select("id").
			Where("kind = ?", database.StoreKindCompany).Take(&company).Error; err != nil {
			httpx.Fail(w, r, httpx.Internal(errors.New("company store is missing")))
			return
		}
		companyStoreID = company.ID
	}

	d, err := s.createDelivery(r.Context(), p, deliveryCreate{
		projectID:     a.ID,
		projectCode:   a.Code,
		projectName:   a.Name,
		fromStoreID:   a.StoreID,
		toStoreID:     companyStoreID,
		fromLabel:     "project_store",
		toLabel:       "company_store",
		viewerStoreID: a.StoreID,
		lines:         req.Lines,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Created(w, d)
}

// handleCompanyCreateDelivery sends material from the company store to a project.
func (s *Server) handleCompanyCreateDelivery(w http.ResponseWriter, r *http.Request) {
	p, companyStoreID, ok := s.requireCompanyStore(w, r)
	if !ok {
		return
	}
	var req createDeliveryRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		httpx.Fail(w, r, httpx.BadRequest("project_id is required"))
		return
	}

	var target projectAccess
	err := s.db.WithContext(r.Context()).
		Table("projects AS p").
		Select("p.id, p.code, p.name, st.id AS store_id").
		Joins("LEFT JOIN stores st ON st.project_id = p.id").
		Where("p.code = upper(?)", strings.TrimSpace(req.ProjectID)).
		Take(&target).Error
	if isNotFound(err) {
		httpx.Fail(w, r, httpx.NotFound("project not found"))
		return
	}
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	if target.StoreID == uuid.Nil {
		httpx.Fail(w, r, httpx.NotFound("project not found"))
		return
	}

	d, err := s.createDelivery(r.Context(), p, deliveryCreate{
		projectID:     target.ID,
		projectCode:   target.Code,
		projectName:   target.Name,
		fromStoreID:   companyStoreID,
		toStoreID:     target.StoreID,
		fromLabel:     "company_store",
		toLabel:       "project_store",
		viewerStoreID: companyStoreID,
		lines:         req.Lines,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.Created(w, d)
}

type deliveryCreate struct {
	projectID     uuid.UUID
	projectCode   string
	projectName   string
	fromStoreID   uuid.UUID
	toStoreID     uuid.UUID
	fromLabel     string
	toLabel       string
	viewerStoreID uuid.UUID
	lines         []deliveryLineRequest
}

func (s *Server) createDelivery(ctx context.Context, p *Principal, in deliveryCreate) (deliveryDTO, error) {
	out := deliveryDTO{ProjectID: in.projectCode, ProjectName: in.projectName}

	if len(in.lines) == 0 {
		return out, httpx.BadRequest("a delivery needs at least one material line")
	}
	if len(in.lines) > 200 {
		return out, httpx.BadRequest("a delivery cannot hold more than 200 material lines")
	}

	type line struct {
		materialID uuid.UUID
		quantity   float64
	}
	// Validate and canonicalise the lines. Sorting by material id makes the stock
	// updates below happen in a stable order, which is what keeps two concurrent
	// deliveries from deadlocking on each other.
	lines := make([]line, 0, len(in.lines))
	seen := map[uuid.UUID]bool{}
	for _, l := range in.lines {
		if l.MaterialID == "" || l.Quantity == nil {
			return out, httpx.BadRequest("each line needs material_id and quantity")
		}
		materialID, err := parseUUID(l.MaterialID)
		if err != nil {
			return out, httpx.BadRequest("each line needs a valid material_id")
		}
		if seen[materialID] {
			return out, httpx.BadRequest("the same material appears twice in one delivery")
		}
		seen[materialID] = true
		q := *l.Quantity
		if math.IsNaN(q) || math.IsInf(q, 0) {
			return out, httpx.BadRequest("quantity must be a number")
		}
		if q <= 0 {
			return out, httpx.BadRequest("quantity must be greater than zero")
		}
		lines = append(lines, line{materialID: materialID, quantity: q})
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].materialID.String() < lines[j].materialID.String() })

	delivery := database.Delivery{
		ID:          uuid.New(),
		ProjectID:   in.projectID,
		FromStoreID: in.fromStoreID,
		ToStoreID:   in.toStoreID,
		Status:      "in_transit",
		CreatedBy:   p.UserID,
		CreatedAt:   time.Now().UTC(),
	}

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		wanted := make([]uuid.UUID, 0, len(lines))
		for _, l := range lines {
			wanted = append(wanted, l.materialID)
		}
		var present int64
		if err := tx.Model(&database.Material{}).Where("id IN ?", wanted).Count(&present).Error; err != nil {
			return err
		}
		if int(present) != len(wanted) {
			return httpx.NotFound("one of the materials does not exist")
		}

		if err := tx.Create(&delivery).Error; err != nil {
			return err
		}
		for _, l := range lines {
			if err := takeStock(ctx, tx, in.fromStoreID, l.materialID, l.quantity); err != nil {
				return err
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&database.DeliveryLine{
				DeliveryID: delivery.ID, MaterialID: l.materialID, Quantity: l.quantity,
			}).Error; err != nil {
				return err
			}
		}
		return touchLastDelivery(ctx, tx, in.fromStoreID, wanted, delivery.CreatedAt)
	})
	if err != nil {
		return out, err
	}

	out.ID = delivery.ID.String()
	out.CreatedAt = delivery.CreatedAt
	out.Status = delivery.Status
	out.CreatedBy = p.Name
	out.From, out.To = in.fromLabel, in.toLabel
	out.AllowedActions = deliveryActions(out.Status, in.toStoreID == in.viewerStoreID)
	if in.fromStoreID == in.viewerStoreID {
		out.Direction = "outgoing"
	} else {
		out.Direction = "incoming"
	}
	out.Lines = []deliveryLineDTO{}
	for _, l := range lines {
		out.Lines = append(out.Lines, deliveryLineDTO{MaterialID: l.materialID.String(), Quantity: l.quantity})
	}
	return out, nil
}

func touchLastDelivery(ctx context.Context, tx *gorm.DB, storeID uuid.UUID, materialIDs []uuid.UUID, at time.Time) error {
	return tx.WithContext(ctx).Model(&database.StoreStock{}).
		Where("store_id = ? AND material_id IN ?", storeID, materialIDs).
		Update("last_delivery_at", at).Error
}

// ---------------------------------------------------------------------------
// transitions
// ---------------------------------------------------------------------------

func (s *Server) handleProjectDeliveryRecord(w http.ResponseWriter, r *http.Request) {
	s.deliveryTransition(w, r, "record", false)
}

func (s *Server) handleProjectDeliveryReject(w http.ResponseWriter, r *http.Request) {
	s.deliveryTransition(w, r, "reject", false)
}

func (s *Server) handleProjectDeliveryCancel(w http.ResponseWriter, r *http.Request) {
	s.deliveryTransition(w, r, "cancel", false)
}

func (s *Server) handleCompanyDeliveryRecord(w http.ResponseWriter, r *http.Request) {
	s.deliveryTransition(w, r, "record", true)
}

func (s *Server) handleCompanyDeliveryReject(w http.ResponseWriter, r *http.Request) {
	s.deliveryTransition(w, r, "reject", true)
}

func (s *Server) handleCompanyDeliveryCancel(w http.ResponseWriter, r *http.Request) {
	s.deliveryTransition(w, r, "cancel", true)
}

// deliveryTransition applies record, reject or cancel. Every branch moves stock
// and writes the delivery status in one transaction, so a delivery is either
// fully applied or not at all.
func (s *Server) deliveryTransition(w http.ResponseWriter, r *http.Request, action string, company bool) {
	p := caller(r)
	ctx := r.Context()

	var viewerStoreID uuid.UUID
	if company {
		_, storeID, ok := s.requireCompanyStore(w, r)
		if !ok {
			return
		}
		viewerStoreID = storeID
	} else {
		a, err := s.projectAccess(ctx, p, r.PathValue("projectCode"), false)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		viewerStoreID = a.StoreID
	}

	deliveryID, err := parseUUID(r.PathValue("deliveryID"))
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("delivery not found"))
		return
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Lock the delivery row first: two clicks on "record" must not double count.
		var delivery database.Delivery
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", deliveryID).Take(&delivery).Error
		if isNotFound(err) {
			return httpx.NotFound("delivery not found")
		}
		if err != nil {
			return err
		}
		if delivery.FromStoreID != viewerStoreID && delivery.ToStoreID != viewerStoreID {
			return httpx.NotFound("delivery not found")
		}
		if delivery.Status != "in_transit" {
			return httpx.Conflict("this delivery is already %s", delivery.Status)
		}

		var newStatus string
		switch action {
		case "record":
			if delivery.ToStoreID != viewerStoreID {
				return httpx.Forbidden("only the receiver can record a delivery")
			}
			newStatus = "delivered"
		case "reject":
			if delivery.ToStoreID != viewerStoreID {
				return httpx.Forbidden("only the receiver can reject a delivery")
			}
			newStatus = "rejected"
		case "cancel":
			if delivery.FromStoreID != viewerStoreID {
				return httpx.Forbidden("only the sender can cancel a delivery")
			}
			newStatus = "cancelled"
		default:
			return httpx.BadRequest("unknown action")
		}

		var lines []database.DeliveryLine
		if err := tx.Where("delivery_id = ?", delivery.ID).Find(&lines).Error; err != nil {
			return err
		}

		now := time.Now().UTC()
		target := delivery.FromStoreID // rejected or cancelled: material goes back
		if newStatus == "delivered" {
			target = delivery.ToStoreID
		}
		ids := make([]uuid.UUID, 0, len(lines))
		for _, l := range lines {
			if err := addStock(ctx, tx, target, l.MaterialID, l.Quantity); err != nil {
				return err
			}
			ids = append(ids, l.MaterialID)
		}
		if err := touchLastDelivery(ctx, tx, target, ids, now); err != nil {
			return err
		}

		return tx.Model(&database.Delivery{}).Where("id = ?", delivery.ID).
			Updates(map[string]any{
				"status": newStatus, "resolved_at": now, "resolved_by": p.UserID,
			}).Error
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if company {
		s.handleGetCompanyDelivery(w, r)
	} else {
		s.handleGetProjectDelivery(w, r)
	}
}

// ---------------------------------------------------------------------------
// single delivery
// ---------------------------------------------------------------------------

func (s *Server) handleGetProjectDelivery(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	s.getDelivery(w, r, a.StoreID, a.ID)
}

func (s *Server) handleGetCompanyDelivery(w http.ResponseWriter, r *http.Request) {
	_, storeID, ok := s.requireCompanyStore(w, r)
	if !ok {
		return
	}
	s.getDelivery(w, r, storeID, uuid.Nil)
}

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request, viewerStoreID, projectID uuid.UUID) {
	deliveryID, err := parseUUID(r.PathValue("deliveryID"))
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("delivery not found"))
		return
	}

	query := s.db.WithContext(r.Context()).
		Table("deliveries AS d").
		Select(`d.id, p.code AS project_code, p.name AS project_name,
		        (d.to_store_id = ?) AS incoming, d.status, d.created_at, d.resolved_at,
		        cu.name AS created_by, ru.name AS resolved_by`, viewerStoreID).
		Joins("JOIN projects p ON p.id = d.project_id").
		Joins("JOIN users cu ON cu.id = d.created_by").
		Joins("LEFT JOIN users ru ON ru.id = d.resolved_by").
		Where("d.id = ? AND (d.from_store_id = ? OR d.to_store_id = ?)", deliveryID, viewerStoreID, viewerStoreID)
	if projectID != uuid.Nil {
		query = query.Where("d.project_id = ?", projectID)
	}

	var row deliveryRow
	if err := query.Take(&row).Error; err != nil {
		httpx.Fail(w, r, httpx.NotFound("delivery not found"))
		return
	}

	d := deliveryDTO{
		ID: row.ID.String(), ProjectID: row.ProjectCode, ProjectName: row.ProjectName,
		Status: row.Status, CreatedAt: row.CreatedAt, ResolvedAt: row.ResolvedAt,
		CreatedBy: row.CreatedBy, ResolvedBy: row.ResolvedBy,
	}
	if row.Incoming {
		d.Direction, d.From, d.To = "incoming", "company_store", "project_store"
	} else {
		d.Direction, d.From, d.To = "outgoing", "project_store", "company_store"
	}
	d.AllowedActions = deliveryActions(d.Status, row.Incoming)

	lines, err := s.deliveryLines(r.Context(), []uuid.UUID{row.ID})
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	d.Lines = lines[d.ID]
	if d.Lines == nil {
		d.Lines = []deliveryLineDTO{}
	}
	httpx.OK(w, d)
}
