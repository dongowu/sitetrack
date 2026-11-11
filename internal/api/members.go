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

type memberDTO struct {
	UserID         string    `json:"user_id"`
	Name           string    `json:"name"`
	Email          string    `json:"email"`
	AppRole        string    `json:"app_role"`
	IsProjectAdmin bool      `json:"is_project_admin"`
	JoinedAt       time.Time `json:"joined_at"`
}

type invitationDTO struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	ProjectName  string    `json:"project_name"`
	InviterName  string    `json:"inviter_name"`
	InviteeID    string    `json:"invitee_id,omitempty"`
	InviteeName  string    `json:"invitee_name,omitempty"`
	InviteeEmail string    `json:"invitee_email,omitempty"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *Server) handleListMembers(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), false)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var rows []memberDTO
	err = s.db.WithContext(r.Context()).
		Table("project_members AS m").
		Select("u.id AS user_id, u.name, u.email, u.app_role, m.is_admin AS is_project_admin, m.joined_at").
		Joins("JOIN users u ON u.id = m.user_id").
		Where("m.project_id = ? AND u.deleted_at IS NULL", a.ID).
		Order("m.is_admin DESC, lower(u.name) ASC").
		Scan(&rows).Error
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	if rows == nil {
		rows = []memberDTO{}
	}

	pending, err := s.pendingInvitationsForProject(r.Context(), a.ID)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.OK(w, map[string]any{
		"project_id":          a.Code,
		"is_project_admin":    a.IsAdmin,
		"members":             rows,
		"pending_invitations": pending,
	})
}

func (s *Server) pendingInvitationsForProject(ctx context.Context, projectID uuid.UUID) ([]invitationDTO, error) {
	var rows []struct {
		ID        uuid.UUID `gorm:"column:id"`
		UserID    uuid.UUID `gorm:"column:user_id"`
		Name      string    `gorm:"column:name"`
		Email     string    `gorm:"column:email"`
		CreatedAt time.Time `gorm:"column:created_at"`
	}
	err := s.db.WithContext(ctx).
		Table("invitations AS i").
		Select("i.id, u.id AS user_id, u.name, u.email, i.created_at").
		Joins("JOIN users u ON u.id = i.invitee_id").
		Where("i.project_id = ? AND i.status = 'pending'", projectID).
		Order("i.created_at DESC").
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}

	out := []invitationDTO{}
	for _, row := range rows {
		out = append(out, invitationDTO{
			ID: row.ID.String(), Status: "pending", CreatedAt: row.CreatedAt,
			InviteeID: row.UserID.String(), InviteeName: row.Name, InviteeEmail: row.Email,
		})
	}
	return out, nil
}

type inviteRequest struct {
	Email  string `json:"email"`
	UserID string `json:"user_id"`
}

func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req inviteRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	ctx := r.Context()

	var invitee database.User
	query := s.db.WithContext(ctx).Where("deleted_at IS NULL")
	switch {
	case req.UserID != "":
		id, err := parseUUID(req.UserID)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		query = query.Where("id = ?", id)
	case req.Email != "":
		query = query.Where("lower(email) = ?", req.Email)
	default:
		httpx.Fail(w, r, httpx.BadRequest("email or user_id is required"))
		return
	}
	if err := query.Take(&invitee).Error; err != nil {
		if isNotFound(err) {
			httpx.Fail(w, r, httpx.NotFound("no registered user with that email"))
			return
		}
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	var isMember int64
	if err := s.db.WithContext(ctx).Model(&database.ProjectMember{}).
		Where("project_id = ? AND user_id = ?", a.ID, invitee.ID).Count(&isMember).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	if isMember > 0 {
		httpx.Fail(w, r, httpx.Conflict("that person is already a member of this project"))
		return
	}

	inv := database.Invitation{
		ID:        uuid.New(),
		ProjectID: a.ID,
		InviterID: p.UserID,
		InviteeID: invitee.ID,
		Status:    "pending",
		CreatedAt: time.Now().UTC(),
	}
	if err := s.db.WithContext(ctx).Create(&inv).Error; err != nil {
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			httpx.Fail(w, r, httpx.Conflict("that person already has an open invitation to this project"))
			return
		}
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.Created(w, invitationDTO{
		ID: inv.ID.String(), ProjectID: a.Code, ProjectName: a.Name, InviterName: p.Name,
		InviteeID: invitee.ID.String(), InviteeName: invitee.Name, InviteeEmail: invitee.Email,
		Status: "pending", CreatedAt: inv.CreatedAt,
	})
}

func (s *Server) handleWithdrawInvitation(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	invitationID, err := parseUUID(r.PathValue("invitationID"))
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("no open invitation with that id"))
		return
	}

	result := s.db.WithContext(r.Context()).Model(&database.Invitation{}).
		Where("id = ? AND project_id = ? AND status = 'pending'", invitationID, a.ID).
		Updates(map[string]any{"status": "withdrawn", "responded_at": time.Now().UTC()})
	if result.Error != nil {
		httpx.Fail(w, r, httpx.Internal(result.Error))
		return
	}
	if result.RowsAffected == 0 {
		httpx.Fail(w, r, httpx.NotFound("no open invitation with that id"))
		return
	}
	httpx.NoContent(w)
}

// loadOpenInvitation finds a pending invitation that belongs to the caller. The
// only actor allowed to answer an invitation is the invitee.
func (s *Server) loadOpenInvitation(ctx context.Context, invitationID uuid.UUID, userID uuid.UUID) (database.Invitation, string, error) {
	var inv database.Invitation
	err := s.db.WithContext(ctx).
		Where("id = ? AND invitee_id = ? AND status = 'pending'", invitationID, userID).
		Take(&inv).Error
	if isNotFound(err) {
		return inv, "", httpx.NotFound("invitation not found")
	}
	if err != nil {
		return inv, "", err
	}

	var project database.Project
	if err := s.db.WithContext(ctx).Select("code").
		Where("id = ?", inv.ProjectID).Take(&project).Error; err != nil {
		return inv, "", err
	}
	return inv, project.Code, nil
}

func (s *Server) handleAcceptInvitation(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	invitationID, err := parseUUID(r.PathValue("invitationID"))
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("invitation not found"))
		return
	}
	inv, projectCode, err := s.loadOpenInvitation(r.Context(), invitationID, p.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		result := tx.Model(&database.Invitation{}).
			Where("id = ? AND invitee_id = ? AND status = 'pending'", inv.ID, p.UserID).
			Updates(map[string]any{"status": "accepted", "responded_at": now, "read_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return httpx.Conflict("invitation was already answered or withdrawn")
		}
		return tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&database.ProjectMember{
				ProjectID: inv.ProjectID, UserID: p.UserID, IsAdmin: false, JoinedAt: now,
			}).Error
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, map[string]any{"project_id": projectCode, "status": "accepted"})
}

func (s *Server) handleRejectInvitation(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	invitationID, err := parseUUID(r.PathValue("invitationID"))
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("invitation not found"))
		return
	}
	inv, projectCode, err := s.loadOpenInvitation(r.Context(), invitationID, p.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	now := time.Now().UTC()
	result := s.db.WithContext(r.Context()).Model(&database.Invitation{}).
		Where("id = ? AND invitee_id = ? AND status = 'pending'", inv.ID, p.UserID).
		Updates(map[string]any{"status": "rejected", "responded_at": now, "read_at": now})
	if result.Error != nil {
		httpx.Fail(w, r, httpx.Internal(result.Error))
		return
	}
	if result.RowsAffected == 0 {
		httpx.Fail(w, r, httpx.Conflict("invitation was already answered or withdrawn"))
		return
	}
	httpx.OK(w, map[string]any{"project_id": projectCode, "status": "rejected"})
}

type updateMemberRequest struct {
	IsProjectAdmin *bool `json:"is_project_admin"`
}

// lockProject serialises membership changes for one project so the "at least one
// admin" check cannot be raced by two concurrent demotions.
func lockProject(tx *gorm.DB, projectID uuid.UUID) error {
	var p database.Project
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", projectID).Take(&p).Error
}

func (s *Server) handleUpdateMember(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	var req updateMemberRequest
	if err := decodeJSON(r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if req.IsProjectAdmin == nil {
		httpx.Fail(w, r, httpx.BadRequest("is_project_admin is required"))
		return
	}
	targetID, err := parseUUID(r.PathValue("userID"))
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("that person is not a member of this project"))
		return
	}

	err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := lockProject(tx, a.ID); err != nil {
			return err
		}

		var member database.ProjectMember
		if err := tx.Where("project_id = ? AND user_id = ?", a.ID, targetID).Take(&member).Error; err != nil {
			if isNotFound(err) {
				return httpx.NotFound("that person is not a member of this project")
			}
			return err
		}

		if !*req.IsProjectAdmin && member.IsAdmin {
			var admins int64
			if err := tx.Model(&database.ProjectMember{}).
				Where("project_id = ? AND is_admin", a.ID).Count(&admins).Error; err != nil {
				return err
			}
			if admins <= 1 {
				return httpx.Conflict("a project must always have at least one project admin")
			}
		}

		return tx.Model(&database.ProjectMember{}).
			Where("project_id = ? AND user_id = ?", a.ID, targetID).
			Update("is_admin", *req.IsProjectAdmin).Error
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.OK(w, map[string]any{"user_id": targetID.String(), "is_project_admin": *req.IsProjectAdmin})
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	a, err := s.projectAccess(r.Context(), p, r.PathValue("projectCode"), true)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	targetID, err := parseUUID(r.PathValue("userID"))
	if err != nil {
		httpx.Fail(w, r, httpx.NotFound("that person is not a member of this project"))
		return
	}

	err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		if err := lockProject(tx, a.ID); err != nil {
			return err
		}

		var member database.ProjectMember
		if err := tx.Where("project_id = ? AND user_id = ?", a.ID, targetID).Take(&member).Error; err != nil {
			if isNotFound(err) {
				return httpx.NotFound("that person is not a member of this project")
			}
			return err
		}

		if member.IsAdmin {
			var admins int64
			if err := tx.Model(&database.ProjectMember{}).
				Where("project_id = ? AND is_admin", a.ID).Count(&admins).Error; err != nil {
				return err
			}
			if admins <= 1 {
				return httpx.Conflict("a project must always have at least one project admin")
			}
		}

		return tx.Where("project_id = ? AND user_id = ?", a.ID, targetID).
			Delete(&database.ProjectMember{}).Error
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w)
}

type notificationRow struct {
	ID          uuid.UUID `gorm:"column:id"`
	ProjectCode string    `gorm:"column:project_code"`
	ProjectName string    `gorm:"column:project_name"`
	InviterName string    `gorm:"column:inviter_name"`
	Status      string    `gorm:"column:status"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	Unread      bool      `gorm:"column:unread"`
}

// handleListNotifications powers screen 3: invitations for the caller, newest
// first, with Accept and Reject available only while pending.
func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	pg := s.paging(r)
	ctx := r.Context()

	status := queryParam(r, "status")
	query := s.db.WithContext(ctx).
		Table("invitations AS i").
		Select(`i.id, p.code AS project_code, p.name AS project_name, u.name AS inviter_name,
		        i.status, i.created_at, (i.read_at IS NULL) AS unread`).
		Joins("JOIN projects p ON p.id = i.project_id").
		Joins("JOIN users u ON u.id = i.inviter_id").
		Where("i.invitee_id = ?", p.UserID)
	if status != "" && status != "all" {
		query = query.Where("i.status = ?", status)
	}

	var rows []notificationRow
	if err := query.Order("i.created_at DESC, i.id DESC").
		Limit(pg.Limit).Offset(pg.Offset).Scan(&rows).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	items := []map[string]any{}
	for _, row := range rows {
		items = append(items, map[string]any{
			"id":           row.ID.String(),
			"type":         "project_invitation",
			"project_id":   row.ProjectCode,
			"project_name": row.ProjectName,
			"inviter_name": row.InviterName,
			"status":       row.Status,
			"unread":       row.Unread,
			"created_at":   row.CreatedAt,
			"can_respond":  row.Status == "pending",
		})
	}

	var total int64
	if err := s.db.WithContext(ctx).Model(&database.Invitation{}).
		Where("invitee_id = ?", p.UserID).Count(&total).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	httpx.List(w, items, map[string]any{
		"total":                total,
		"limit":                pg.Limit,
		"offset":               pg.Offset,
		"unread_notifications": s.unreadCount(ctx, p.UserID),
	})
}

func (s *Server) handleMarkNotificationsRead(w http.ResponseWriter, r *http.Request) {
	p := caller(r)
	if err := s.db.WithContext(r.Context()).Model(&database.Invitation{}).
		Where("invitee_id = ? AND read_at IS NULL", p.UserID).
		Update("read_at", time.Now().UTC()).Error; err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.OK(w, map[string]any{"unread_notifications": 0})
}

// handleSearchUsers lets a project admin find somebody to invite by name or
// email. It only exposes id, name and email, and only to a valid caller.
func (s *Server) handleSearchUsers(w http.ResponseWriter, r *http.Request) {
	q := queryParam(r, "q")
	if len(q) < 2 {
		httpx.Fail(w, r, httpx.BadRequest("q must be at least 2 characters"))
		return
	}
	limit := 20

	var rows []struct {
		ID      uuid.UUID `gorm:"column:id"`
		Name    string    `gorm:"column:name"`
		Email   string    `gorm:"column:email"`
		AppRole string    `gorm:"column:app_role"`
	}
	err := s.db.WithContext(r.Context()).Model(&database.User{}).
		Select("id, name, email, app_role").
		Where("deleted_at IS NULL").
		Where("lower(name) LIKE ? OR lower(email) LIKE ?", likePattern(q), likePattern(q)).
		Order("lower(name)").Limit(limit).Scan(&rows).Error
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}

	out := []map[string]any{}
	for _, row := range rows {
		out = append(out, map[string]any{
			"id": row.ID.String(), "name": row.Name, "email": row.Email, "app_role": row.AppRole,
		})
	}
	httpx.List(w, out, map[string]any{"limit": limit})
}
