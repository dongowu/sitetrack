// Package database owns the connection and the schema.
//
// Migrations are plain SQL files embedded in the binary and applied in filename
// order inside a transaction. A tiny schema_migrations table records what has
// run, so a restart is a no-op. This keeps `docker compose up` to a single step:
// no separate migrate container, no CLI to install.
//
// Everything this package does at runtime goes through GORM. The two exceptions
// are deliberate and marked where they occur: the body of a migration file is
// arbitrary DDL by definition, so it can only be handed to the driver as text.
package database

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// schemaMigration is the runner's own bookkeeping. It is a GORM model like any
// other, so creating it, reading it and recording a version are ordinary
// Create/Count calls rather than hand-written SQL.
type schemaMigration struct {
	Version   string    `gorm:"column:version;primaryKey"`
	AppliedAt time.Time `gorm:"column:applied_at;not null;autoCreateTime"`
}

func (schemaMigration) TableName() string { return "schema_migrations" }

// Connect opens the GORM handle and waits for Postgres to accept connections,
// because the API container may start before the database is ready.
func Connect(ctx context.Context, url string) (*gorm.DB, error) {
	var lastErr error
	for attempt := 1; attempt <= 30; attempt++ {
		db, err := gorm.Open(postgres.Open(url), &gorm.Config{
			// Turn driver errors into gorm.ErrDuplicatedKey and
			// gorm.ErrForeignKeyViolated so handlers do not have to parse strings.
			TranslateError: true,
			Logger:         logger.Default.LogMode(logger.Silent),
			NowFunc:        func() time.Time { return time.Now().UTC() },
		})
		if err == nil {
			sqlDB, err := db.DB()
			if err == nil {
				sqlDB.SetMaxOpenConns(20)
				sqlDB.SetMaxIdleConns(4)
				sqlDB.SetConnMaxLifetime(time.Hour)
				if err = sqlDB.PingContext(ctx); err == nil {
					return db, nil
				}
			}
			lastErr = err
		} else {
			lastErr = err
		}

		slog.Warn("waiting for database", "attempt", attempt, "err", lastErr)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("database unreachable: %w", lastErr)
}

// Migrate applies every embedded migration that has not run yet, in filename
// order, each inside its own transaction together with the row that records it.
func Migrate(ctx context.Context, db *gorm.DB) error {
	if err := db.WithContext(ctx).AutoMigrate(&schemaMigration{}); err != nil {
		return fmt.Errorf("prepare schema_migrations: %w", err)
	}

	entries, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)

	for _, name := range entries {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")

		var applied int64
		if err := db.WithContext(ctx).Model(&schemaMigration{}).
			Where("version = ?", version).Count(&applied).Error; err != nil {
			return err
		}
		if applied > 0 {
			continue
		}

		body, err := migrationFiles.ReadFile(name)
		if err != nil {
			return err
		}

		err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// A migration file is raw DDL - partial indexes, CHECK constraints,
			// triggers and sequence defaults that no model tag can express - so it
			// goes to the driver as text. Everything around it is GORM.
			if err := tx.Exec(string(body)).Error; err != nil {
				return fmt.Errorf("migration %s: %w", version, err)
			}
			return tx.Create(&schemaMigration{Version: version}).Error
		})
		if err != nil {
			return err
		}
		slog.Info("applied migration", "version", version)
	}
	return nil
}

// EnsureCompanyStore creates the single central warehouse if it is missing.
// The stores_single_company partial unique index makes this a no-op on every
// start after the first.
func EnsureCompanyStore(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).
		Clauses(clause.OnConflict{DoNothing: true}).
		Create(&Store{
			ID:        uuid.New(),
			Kind:      StoreKindCompany,
			CreatedAt: time.Now().UTC(),
		}).Error
}
