// Package database owns the connection and the schema.
//
// Migrations are plain SQL files embedded in the binary and applied in filename
// order inside a transaction. A tiny schema_migrations table records what has
// run, so a restart is a no-op. This keeps `docker compose up` to a single step:
// no separate migrate container, no CLI to install.
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

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

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

func Migrate(ctx context.Context, db *gorm.DB) error {
	if err := db.WithContext(ctx).Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`).Error; err != nil {
		return err
	}

	entries, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)

	for _, name := range entries {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")

		var applied int64
		if err := db.WithContext(ctx).Raw(
			`SELECT count(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied).Error; err != nil {
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
			if err := tx.Exec(string(body)).Error; err != nil {
				return fmt.Errorf("migration %s: %w", version, err)
			}
			return tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version).Error
		})
		if err != nil {
			return err
		}
		slog.Info("applied migration", "version", version)
	}
	return nil
}

// EnsureCompanyStore creates the single central warehouse if it is missing.
func EnsureCompanyStore(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Exec(`
		INSERT INTO stores (id, kind) VALUES (gen_random_uuid(), 'company')
		ON CONFLICT DO NOTHING`).Error
}
