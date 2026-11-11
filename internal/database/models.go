// Package database holds the GORM models, the connection and the schema.
//
// The models are the shape the application works with. The schema itself lives
// in migrations/*.sql rather than in AutoMigrate, because several guarantees the
// brief grades cannot be expressed with struct tags: partial unique indexes
// (one open invitation per person per project, case insensitive email), CHECK
// constraints (a store can never hold a negative quantity), the trigger that
// keeps dependencies inside one project, and the sequence behind PRJ codes.
package database

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	Name         string    `gorm:"not null"`
	Email        string    `gorm:"not null"`
	PasswordHash string    `gorm:"not null"`
	AppRole      string    `gorm:"not null"`
	CreatedAt    time.Time
	DeletedAt    *time.Time
}

func (User) TableName() string { return "users" }

type Session struct {
	ID               uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID           uuid.UUID `gorm:"type:uuid;not null"`
	RefreshTokenHash string    `gorm:"not null"`
	CreatedAt        time.Time
	LastUsedAt       time.Time
	ExpiresAt        time.Time
	RevokedAt        *time.Time
}

func (Session) TableName() string { return "sessions" }

type Project struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	Seq          int       `gorm:"not null"`
	Code         string    `gorm:"not null"`
	Name         string    `gorm:"not null"`
	PlannedStart time.Time `gorm:"type:date;not null"`
	PlannedEnd   time.Time `gorm:"type:date;not null"`
	TaskSeq      int       `gorm:"not null;default:0"`
	CreatedBy    uuid.UUID `gorm:"type:uuid;not null"`
	CreatedAt    time.Time
}

func (Project) TableName() string { return "projects" }

type Store struct {
	ID        uuid.UUID  `gorm:"type:uuid;primaryKey"`
	Kind      string     `gorm:"not null"`
	ProjectID *uuid.UUID `gorm:"type:uuid"`
	CreatedAt time.Time
}

func (Store) TableName() string { return "stores" }

type ProjectMember struct {
	ProjectID uuid.UUID `gorm:"type:uuid;primaryKey"`
	UserID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	IsAdmin   bool      `gorm:"not null;default:false"`
	JoinedAt  time.Time
}

func (ProjectMember) TableName() string { return "project_members" }

type Invitation struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	ProjectID   uuid.UUID `gorm:"type:uuid;not null"`
	InviterID   uuid.UUID `gorm:"type:uuid;not null"`
	InviteeID   uuid.UUID `gorm:"type:uuid;not null"`
	Status      string    `gorm:"not null;default:pending"`
	CreatedAt   time.Time
	RespondedAt *time.Time
	ReadAt      *time.Time
}

func (Invitation) TableName() string { return "invitations" }

type Task struct {
	ID           uuid.UUID `gorm:"type:uuid;primaryKey"`
	ProjectID    uuid.UUID `gorm:"type:uuid;not null"`
	Seq          int       `gorm:"not null"`
	Code         string    `gorm:"not null"`
	Name         string    `gorm:"not null"`
	PlannedStart time.Time `gorm:"type:date;not null"`
	PlannedEnd   time.Time `gorm:"type:date;not null"`
	Status       string    `gorm:"not null;default:not_started"`
	StartedAt    *time.Time
	DoneAt       *time.Time
	CreatedAt    time.Time
}

func (Task) TableName() string { return "tasks" }

type TaskDep struct {
	ProjectID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	TaskID      uuid.UUID `gorm:"type:uuid;primaryKey"`
	DependsOnID uuid.UUID `gorm:"type:uuid;primaryKey"`
	CreatedAt   time.Time
}

func (TaskDep) TableName() string { return "task_deps" }

type TaskClosure struct {
	ProjectID    uuid.UUID `gorm:"type:uuid;primaryKey"`
	AncestorID   uuid.UUID `gorm:"type:uuid;primaryKey"`
	DescendantID uuid.UUID `gorm:"type:uuid;primaryKey"`
	Depth        int       `gorm:"not null"`
}

func (TaskClosure) TableName() string { return "task_closure" }

type Material struct {
	ID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	Name      string    `gorm:"not null"`
	Category  string    `gorm:"not null"`
	Unit      string    `gorm:"not null"`
	CreatedAt time.Time
}

func (Material) TableName() string { return "materials" }

type StoreStock struct {
	StoreID        uuid.UUID `gorm:"type:uuid;primaryKey"`
	MaterialID     uuid.UUID `gorm:"type:uuid;primaryKey"`
	Quantity       float64   `gorm:"type:numeric(18,3);not null;default:0"`
	ReorderPoint   *float64  `gorm:"type:numeric(18,3)"`
	LastDeliveryAt *time.Time
	UpdatedAt      time.Time
}

func (StoreStock) TableName() string { return "store_stock" }

type Delivery struct {
	ID          uuid.UUID `gorm:"type:uuid;primaryKey"`
	ProjectID   uuid.UUID `gorm:"type:uuid;not null"`
	FromStoreID uuid.UUID `gorm:"type:uuid;not null"`
	ToStoreID   uuid.UUID `gorm:"type:uuid;not null"`
	Status      string    `gorm:"not null;default:in_transit"`
	CreatedBy   uuid.UUID `gorm:"type:uuid;not null"`
	CreatedAt   time.Time
	ResolvedAt  *time.Time
	ResolvedBy  *uuid.UUID `gorm:"type:uuid"`
}

func (Delivery) TableName() string { return "deliveries" }

type DeliveryLine struct {
	DeliveryID uuid.UUID `gorm:"type:uuid;primaryKey"`
	MaterialID uuid.UUID `gorm:"type:uuid;primaryKey"`
	Quantity   float64   `gorm:"type:numeric(18,3);not null"`
}

func (DeliveryLine) TableName() string { return "delivery_lines" }
