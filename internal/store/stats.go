package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/qist/sqlite"
)

// DBStats is a snapshot of SQLite's per-connection runtime counters exposed by
// sqlite3_db_status. They are cheap to read and are the only way to see page
// cache behaviour, lookaside pressure and spill without a profiler.
//
// Counters are cumulative for the connection that served the snapshot. With
// the single-connection pool opened by Open that means the whole process.
type DBStats struct {
	CacheHits     int // page cache hits
	CacheMisses   int // page cache misses
	CacheWrites   int // dirty pages written to WAL
	CacheSpills   int // dirty pages spilled early because the cache was full
	TempSpill     int // bytes spilled from temp buffers to temp files
	LookasideUsed int // lookaside memory in use now
	LookasideHigh int // lookaside high-water mark
	SchemaBytes   int // memory used by the schema
	StmtBytes     int // memory used by prepared statements
	DeferredFKs   bool
}

// HitRate is the page-cache hit ratio in [0,1]; 0 before any page read.
func (s DBStats) HitRate() float64 {
	total := s.CacheHits + s.CacheMisses
	if total == 0 {
		return 0
	}
	return float64(s.CacheHits) / float64(total)
}

// DBStatus reads SQLite's runtime counters. When reset is true the counters
// are zeroed after the read, which is how a poller turns them into rates.
//
// Do not call this from inside a transaction: it needs its own connection and
// the single-connection pool is held by the transaction.
func (s *Store) DBStatus(ctx context.Context, reset bool) (DBStats, error) {
	sqlDB, err := s.DB.DB()
	if err != nil {
		return DBStats{}, fmt.Errorf("sqlite pool: %w", err)
	}

	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return DBStats{}, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	var out DBStats
	err = conn.Raw(func(dc any) error {
		status, ok := dc.(sqlite.DBStatus)
		if !ok {
			return errors.New("sqlite driver does not expose DBStatus")
		}
		read := func(op sqlite.DBStatusOp) (int, error) {
			current, _, err := status.Status(op, reset)
			return current, err
		}

		if out.CacheHits, err = read(sqlite.DBStatusCacheHit); err != nil {
			return err
		}
		if out.CacheMisses, err = read(sqlite.DBStatusCacheMiss); err != nil {
			return err
		}
		if out.CacheWrites, err = read(sqlite.DBStatusCacheWrite); err != nil {
			return err
		}
		if out.CacheSpills, err = read(sqlite.DBStatusCacheSpill); err != nil {
			return err
		}
		if out.TempSpill, err = read(sqlite.DBStatusTempbufSpill); err != nil {
			return err
		}
		if out.LookasideUsed, out.LookasideHigh, err = status.Status(sqlite.DBStatusLookasideUsed, reset); err != nil {
			return err
		}
		if out.SchemaBytes, err = read(sqlite.DBStatusSchemaUsed); err != nil {
			return err
		}
		if out.StmtBytes, err = read(sqlite.DBStatusStmtUsed); err != nil {
			return err
		}
		deferred, err := read(sqlite.DBStatusDeferredFKs)
		if err != nil {
			return err
		}
		out.DeferredFKs = deferred != 0
		return nil
	})
	if err != nil {
		return DBStats{}, fmt.Errorf("read db status: %w", err)
	}
	return out, nil
}
