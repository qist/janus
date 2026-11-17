// Package store owns persistence: GORM models, migrations, and repositories.
// It does not contain HTTP or work-board decisions.
package store

import (
	"errors"
	"fmt"
	"strings"

	"github.com/qist/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// ErrVersionConflict means an optimistic-lock update lost the race.
var ErrVersionConflict = errors.New("work item version conflict")

// Store is the database handle shared by repositories.
type Store struct {
	DB *gorm.DB
}

// Open opens (or creates) the SQLite database and runs the v2 migrations.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path required")
	}

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	if err := db.Exec("PRAGMA journal_mode=WAL").Error; err != nil {
		return nil, fmt.Errorf("enable wal: %w", err)
	}
	if err := db.Exec("PRAGMA busy_timeout=5000").Error; err != nil {
		return nil, fmt.Errorf("set busy_timeout: %w", err)
	}
	if err := db.AutoMigrate(
		&WorkItemModel{},
		&WorkClaimModel{},
		&WorkArtifactModel{},
		&WorkReviewModel{},
		&WorkEventModel{},
	); err != nil {
		return nil, fmt.Errorf("migrate work tables: %w", err)
	}

	return &Store{DB: db}, nil
}

// Close releases the underlying SQL connection.
func (s *Store) Close() error {
	if s == nil || s.DB == nil {
		return nil
	}
	sqlDB, err := s.DB.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
