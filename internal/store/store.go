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

	// Pragmas must be set per connection; a one-off PRAGMA exec only affects
	// whichever pooled connection happened to serve it.
	//
	// _txlock=immediate is the important one: every gorm transaction starts
	// with BEGIN IMMEDIATE, taking the write lock up front. Two deferred
	// transactions that read first and then write cannot both upgrade, and
	// one gets SQLITE_BUSY immediately instead of waiting; immediate avoids
	// that class of failure entirely and lets busy_timeout do its job.
	dsn := path
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	dsn += sep + "_busy_timeout=5000&_journal_mode=wal&_txlock=immediate"

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("open sqlite pool: %w", err)
	}
	// SQLite still has one writer at a time. Keeping a single connection makes
	// writes serial in-process and makes DBStatus() counters exact (they are
	// per connection). With _txlock=immediate, raising this later is safe;
	// multiple processes on the same file fall back to busy_timeout.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := db.AutoMigrate(
		&WorkItemModel{},
		&WorkClaimModel{},
		&WorkArtifactModel{},
		&WorkReviewModel{},
		&WorkEventModel{},
		&WorkCapsuleModel{},
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
