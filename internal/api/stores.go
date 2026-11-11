package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dongowu/sitetrack/internal/database"
	"github.com/dongowu/sitetrack/internal/httpx"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type materialDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Unit     string `json:"unit"`
}

// ---------------------------------------------------------------------------
// catalogue
// ---------------------------------------------------------------------------

func (s *Server) handleListMaterials(w http.ResponseWriter, r *http.Request) {
	pg := s.paging(r)
	ctx := r.Context()

	// These expressions are used with Model(&Material{}), whose implicit alias is
	// "materials" rather than a short "m" — keep them unqualified so the two stay
	// in sync.
	orderBy := map[string]string{
		"name":     "lower(name)",
		"category": "lower(category)",
		"unit":     "lower(unit)",
	}
	expr, ok := orderBy[queryParam(r, "sort")]
	if !ok {
		expr = orderBy["name"]
	}

	query := s.db.WithContext(ctx).Model(&database.Material{})
	if q := queryParam(r, "q"); q != "" {
		query = query.Where("lower(name) LIKE ?", likePattern(q))
	}
	if cat := queryParam(r, "category"); cat != "" {
		query = query.Where("lower(category) = lower(?)", cat)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	var materials []database.Material
	if err := query.Clauses(ordering(expr, direction(queryParam(r, "dir")))).
		Limit(pg.Limit).Offset(pg.Offset).Find(&materials).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	items := make([]materialDTO, 0, len(materials))
	for _, m := range materials {
		items = append(items, materialDTO{ID: m.ID.String(), Name: m.Name, Category: m.Category, Unit: m.Unit})
	}
	httpx.List(w, items, map[string]any{"total": total, "limit": pg.Limit, "offset": pg.Offset})
}

type materialRequest struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Unit     string `json:"unit"`
}

func (s *Server) handleCreateMaterial(w http.ResponseWriter, r *http.Request) {
	var req materialRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Category = strings.TrimSpace(req.Category)
	req.Unit = strings.TrimSpace(req.Unit)
	if req.Name == "" || req.Category == "" || req.Unit == "" {
		httpx.Fail(w, r, httpx.BadRequest("name, category and unit are required"))
		return
	}

	m := database.Material{
		ID: uuid.New(), Name: req.Name, Category: req.Category, Unit: req.Unit, CreatedAt: time.Now().UTC(),
	}
	if err := s.db.WithContext(r.Context()).Create(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			httpx.Fail(w, r, httpx.Conflict("that material already exists in the catalogue"))
			return
		}
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.Created(w, materialDTO{ID: m.ID.String(), Name: m.Name, Category: m.Category, Unit: m.Unit})
}

func (s *Server) handleUpdateMaterial(w http.ResponseWriter, r *http.Request) {
	var req materialRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	materialID, err := parseUUID(r.PathValue("materialID"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	updates := map[string]any{}
	if v := strings.TrimSpace(req.Name); v != "" {
		updates["name"] = v
	}
	if v := strings.TrimSpace(req.Category); v != "" {
		updates["category"] = v
	}
	if v := strings.TrimSpace(req.Unit); v != "" {
		updates["unit"] = v
	}
	if len(updates) == 0 {
		httpx.Fail(w, r, httpx.BadRequest("nothing to update"))
		return
	}

	result := s.db.WithContext(r.Context()).Model(&database.Material{}).
		Where("id = ?", materialID).Updates(updates)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrDuplicatedKey) {
			httpx.Fail(w, r, httpx.Conflict("that material name is already taken"))
			return
		}
		httpx.Fail(w, r, httpx.Internal(result.Error))
		return
	}
	if result.RowsAffected == 0 {
		httpx.Fail(w, r, httpx.NotFound("material not found"))
		return
	}

	var m database.Material
	if err := s.db.WithContext(r.Context()).Where("id = ?", materialID).Take(&m).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.OK(w, materialDTO{ID: m.ID.String(), Name: m.Name, Category: m.Category, Unit: m.Unit})
}

// handleDeleteMaterial refuses while stock or delivery lines still reference the
// material: silently dropping history is worse than a clear conflict.
func (s *Server) handleDeleteMaterial(w http.ResponseWriter, r *http.Request) {
	materialID, err := parseUUID(r.PathValue("materialID"))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	result := s.db.WithContext(r.Context()).Where("id = ?", materialID).Delete(&database.Material{})
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrForeignKeyViolated) {
			httpx.Fail(w, r, httpx.Conflict("this material is still used by a store or a delivery"))
			return
		}
		httpx.Fail(w, r, httpx.Internal(result.Error))
		return
	}
	if result.RowsAffected == 0 {
		httpx.Fail(w, r, httpx.NotFound("material not found"))
		return
	}
	httpx.NoContent(w)
}

// ---------------------------------------------------------------------------
// stock views
// ---------------------------------------------------------------------------

type stockItemDTO struct {
	MaterialID        string     `json:"material_id"`
	Name              string     `json:"name"`
	Category          string     `json:"category"`
	Unit              string     `json:"unit"`
	Quantity          float64    `json:"quantity"`
	ReorderPoint      *float64   `json:"reorder_point,omitempty"`
	Status            string     `json:"status,omitempty"`
	QuantityInTransit *float64   `json:"quantity_in_transit,omitempty"`
	LastDeliveryAt    *time.Time `json:"last_delivery_at"`
}

type stockRow struct {
	MaterialID     uuid.UUID  `gorm:"column:material_id"`
	Name           string     `gorm:"column:name"`
	Category       string     `gorm:"column:category"`
	Unit           string     `gorm:"column:unit"`
	Quantity       float64    `gorm:"column:quantity"`
	ReorderPoint   *float64   `gorm:"column:reorder_point"`
	Status         string     `gorm:"column:status"`
	LastDeliveryAt *time.Time `gorm:"column:last_delivery_at"`
}

func (s *Server) handleProjectStore(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	pg := s.paging(r)
	ctx := r.Context()

	orderBy := map[string]string{
		"name":          "lower(m.name)",
		"quantity":      "s.quantity",
		"last_delivery": "s.last_delivery_at",
		"category":      "lower(m.category)",
	}
	expr, ok := orderBy[queryParam(r, "sort")]
	if !ok {
		expr = orderBy["name"]
	}

	query := s.db.WithContext(ctx).
		Table("store_stock AS s").
		Joins("JOIN materials m ON m.id = s.material_id").
		Where("s.store_id = ?", a.StoreID)
	if q := queryParam(r, "q"); q != "" {
		query = query.Where("lower(m.name) LIKE ?", likePattern(q))
	}
	if cat := queryParam(r, "category"); cat != "" {
		query = query.Where("lower(m.category) = lower(?)", cat)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	var rows []stockRow
	if err := query.
		Select(`m.id AS material_id, m.name, m.category, m.unit,
		        s.quantity::float8 AS quantity, s.last_delivery_at`).
		Clauses(ordering(expr, direction(queryParam(r, "dir"))+" NULLS LAST")).
		Order("lower(m.name)").Limit(pg.Limit).Offset(pg.Offset).Scan(&rows).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	items := make([]stockItemDTO, 0, len(rows))
	for _, row := range rows {
		items = append(items, stockItemDTO{
			MaterialID: row.MaterialID.String(), Name: row.Name, Category: row.Category,
			Unit: row.Unit, Quantity: row.Quantity, LastDeliveryAt: row.LastDeliveryAt,
		})
	}

	httpx.List(w, items, map[string]any{
		"total": total, "limit": pg.Limit, "offset": pg.Offset,
		"unread_notifications": s.unreadCount(ctx, p.UserID),
	})
}

// requireCompanyStore gates every /company/* endpoint.
func (s *Server) requireCompanyStore(w http.ResponseWriter, r *http.Request) (*Principal, uuid.UUID, bool) {
	p := caller(r)
	if !p.CanUseCompanyStore() {
		httpx.Fail(w, r, httpx.Forbidden("only a company store manager can reach the company store"))
		return nil, uuid.Nil, false
	}
	var company database.Store
	if err := s.db.WithContext(r.Context()).
		Select("id").Where("kind = ?", database.StoreKindCompany).
		Take(&company).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(errors.New("company store is missing")))
		return nil, uuid.Nil, false
	}
	return p, company.ID, true
}

// statusExpr mirrors the three states the brief defines. It is repeated in the
// WHERE and ORDER BY clauses because Postgres will not accept an output alias
// there.
const statusExpr = `(CASE
	WHEN s.quantity = 0 THEN 'out_of_stock'
	WHEN s.quantity <= COALESCE(s.reorder_point, 0) THEN 'low_stock'
	ELSE 'available' END)`

func (s *Server) handleCompanyStore(w http.ResponseWriter, r *http.Request) {
	_, storeID, ok := s.requireCompanyStore(w, r)
	if !ok {
		return
	}
	pg := s.paging(r)
	ctx := r.Context()

	status := strings.ToLower(queryParam(r, "status"))
	switch status {
	case "", "out_of_stock", "low_stock", "available":
	default:
		httpx.Fail(w, r, httpx.BadRequest("status must be out_of_stock, low_stock or available"))
		return
	}

	orderBy := map[string]string{
		"name":     "lower(m.name)",
		"quantity": "s.quantity",
		"status":   statusExpr,
		"category": "lower(m.category)",
	}
	expr, ok := orderBy[queryParam(r, "sort")]
	if !ok {
		expr = orderBy["name"]
	}

	query := s.db.WithContext(ctx).
		Table("store_stock AS s").
		Joins("JOIN materials m ON m.id = s.material_id").
		Where("s.store_id = ?", storeID)
	if q := queryParam(r, "q"); q != "" {
		query = query.Where("lower(m.name) LIKE ?", likePattern(q))
	}
	if status != "" {
		query = query.Where(statusExpr+" = ?", status)
	}
	if cat := queryParam(r, "category"); cat != "" {
		query = query.Where("lower(m.category) = lower(?)", cat)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	var rows []stockRow
	if err := query.
		Select(`m.id AS material_id, m.name, m.category, m.unit,
		        s.quantity::float8 AS quantity, s.reorder_point::float8 AS reorder_point,
		        ` + statusExpr + ` AS status,
		        s.last_delivery_at`).
		Clauses(ordering(expr, direction(queryParam(r, "dir"))+" NULLS LAST")).
		Order("lower(m.name)").Limit(pg.Limit).Offset(pg.Offset).Scan(&rows).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	materialIDs := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		materialIDs = append(materialIDs, row.MaterialID)
	}
	transit, err := s.inTransitByMaterial(ctx, storeID, materialIDs)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	items := make([]stockItemDTO, 0, len(rows))
	for _, row := range rows {
		inTransit := transit[row.MaterialID]
		items = append(items, stockItemDTO{
			MaterialID: row.MaterialID.String(), Name: row.Name, Category: row.Category,
			Unit: row.Unit, Quantity: row.Quantity, ReorderPoint: row.ReorderPoint,
			Status: row.Status, QuantityInTransit: &inTransit, LastDeliveryAt: row.LastDeliveryAt,
		})
	}
	httpx.List(w, items, map[string]any{"total": total, "limit": pg.Limit, "offset": pg.Offset})
}

// inTransitByMaterial reports how much of each material is sitting on a truck
// that left storeID. Project stores do not expose this figure, so it is only
// paid for on the company store.
//
// This is a second, batched query rather than a correlated subquery inside the
// page query on purpose. The company store is the source of ~180k deliveries
// (~13k of them still in transit at full scale), so a correlated subquery has no
// good plan: driven per output row it re-scans every in-transit delivery for the
// store once per row (measured 1.1s for a 20-row page), and pre-aggregating the
// whole store before paging costs ~150ms even though 20 rows are needed.
//
// Asking for the 20 material ids on the page instead hits
// delivery_lines_material_idx directly. OFFSET 0 stops the planner from pulling
// the delivery subquery up into a nested loop and forces a hash semi-join over
// the store's pending deliveries, which is what keeps this at a few ms.
func (s *Server) inTransitByMaterial(ctx context.Context, storeID uuid.UUID, materialIDs []uuid.UUID) (map[uuid.UUID]float64, error) {
	out := make(map[uuid.UUID]float64, len(materialIDs))
	if len(materialIDs) == 0 {
		return out, nil
	}

	var sums []struct {
		MaterialID uuid.UUID `gorm:"column:material_id"`
		Quantity   float64   `gorm:"column:quantity"`
	}
	err := s.db.WithContext(ctx).
		Table("delivery_lines AS l").
		Select("l.material_id, sum(l.quantity)::float8 AS quantity").
		Joins(`JOIN (
			SELECT id FROM deliveries
			WHERE from_store_id = ? AND status = 'in_transit'
			OFFSET 0
		) d ON d.id = l.delivery_id`, storeID).
		Where("l.material_id IN ?", materialIDs).
		Group("l.material_id").
		Scan(&sums).Error
	if err != nil {
		return nil, err
	}
	for _, sum := range sums {
		out[sum.MaterialID] = sum.Quantity
	}
	return out, nil
}

type addStockRequest struct {
	MaterialID string   `json:"material_id"`
	Quantity   *float64 `json:"quantity"`
}

// handleAddStock is the only door through which new material enters SiteTrack.
func (s *Server) handleAddStock(w http.ResponseWriter, r *http.Request) {
	_, storeID, ok := s.requireCompanyStore(w, r)
	if !ok {
		return
	}
	var req addStockRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.MaterialID == "" {
		httpx.Fail(w, r, httpx.BadRequest("material_id is required"))
		return
	}
	if req.Quantity == nil || *req.Quantity <= 0 {
		httpx.Fail(w, r, httpx.BadRequest("quantity must be greater than zero"))
		return
	}
	materialID, err := parseUUID(req.MaterialID)
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("material not found"))
		return
	}

	ctx := r.Context()
	var exists int64
	if err := s.db.WithContext(ctx).Model(&database.Material{}).
		Where("id = ?", materialID).Count(&exists).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	if exists == 0 {
		httpx.Fail(w, r, httpx.NotFound("material not found"))
		return
	}

	if err := addStock(ctx, s.db, storeID, materialID, *req.Quantity); err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	item, err := s.stockItem(ctx, storeID, materialID)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.OK(w, item)
}

type reorderPointRequest struct {
	ReorderPoint *float64 `json:"reorder_point"`
}

func (s *Server) handleSetReorderPoint(w http.ResponseWriter, r *http.Request) {
	_, storeID, ok := s.requireCompanyStore(w, r)
	if !ok {
		return
	}
	var req reorderPointRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.ReorderPoint != nil && *req.ReorderPoint < 0 {
		httpx.Fail(w, r, httpx.BadRequest("reorder_point cannot be negative"))
		return
	}
	materialID, err := parseUUID(r.PathValue("materialID"))
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("material not found"))
		return
	}

	ctx := r.Context()
	now := time.Now().UTC()
	// The material must already be stocked for its reorder point to mean
	// anything, so this upserts a zero quantity row when needed.
	err = s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "store_id"}, {Name: "material_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"reorder_point": req.ReorderPoint,
			"updated_at":    now,
		}),
	}).Create(&database.StoreStock{
		StoreID: storeID, MaterialID: materialID, Quantity: 0,
		ReorderPoint: req.ReorderPoint, UpdatedAt: now,
	}).Error
	if err != nil {
		if errors.Is(err, gorm.ErrForeignKeyViolated) {
			httpx.Fail(w, r, httpx.NotFound("material not found"))
			return
		}
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	item, err := s.stockItem(ctx, storeID, materialID)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.OK(w, item)
}

func (s *Server) stockItem(ctx context.Context, storeID, materialID uuid.UUID) (stockItemDTO, error) {
	var row stockRow
	err := s.db.WithContext(ctx).
		Table("store_stock AS s").
		Joins("JOIN materials m ON m.id = s.material_id").
		Select(`m.id AS material_id, m.name, m.category, m.unit,
		        s.quantity::float8 AS quantity, s.reorder_point::float8 AS reorder_point,
		        `+statusExpr+` AS status, s.last_delivery_at`).
		Where("s.store_id = ? AND s.material_id = ?", storeID, materialID).
		Scan(&row).Error
	if err != nil {
		return stockItemDTO{}, err
	}
	return stockItemDTO{
		MaterialID: row.MaterialID.String(), Name: row.Name, Category: row.Category,
		Unit: row.Unit, Quantity: row.Quantity, ReorderPoint: row.ReorderPoint,
		Status: row.Status, LastDeliveryAt: row.LastDeliveryAt,
	}, nil
}

// addStock applies a positive delta through an upsert. Called inside a
// transaction when it is part of a delivery.
func addStock(ctx context.Context, db *gorm.DB, storeID, materialID uuid.UUID, qty float64) error {
	now := time.Now().UTC()
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "store_id"}, {Name: "material_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"quantity":   gorm.Expr("store_stock.quantity + EXCLUDED.quantity"),
			"updated_at": now,
		}),
	}).Create(&database.StoreStock{
		StoreID: storeID, MaterialID: materialID, Quantity: qty, UpdatedAt: now,
	}).Error
}

// takeStock removes stock only if it is there. The quantity condition lives in
// the UPDATE itself, so two concurrent senders can never drive a balance below
// zero.
func takeStock(ctx context.Context, tx *gorm.DB, storeID, materialID uuid.UUID, qty float64) error {
	result := tx.WithContext(ctx).Model(&database.StoreStock{}).
		Where("store_id = ? AND material_id = ? AND quantity >= ?", storeID, materialID, qty).
		Updates(map[string]any{
			"quantity":   gorm.Expr("quantity - ?", qty),
			"updated_at": time.Now().UTC(),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return httpx.Conflict("not enough stock for that material in this store")
	}
	return nil
}
