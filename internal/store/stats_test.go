package store

import (
	"context"
	"testing"
)

func TestDBStatusReportsCacheActivity(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	for _, id := range []string{"w1", "w2", "w3"} {
		if err := s.CreateWorkItem(ctx, sampleItem(id)); err != nil {
			t.Fatalf("CreateWorkItem(%s): %v", id, err)
		}
	}
	for _, id := range []string{"w1", "w2", "w3"} {
		if _, err := s.GetWorkItem(ctx, id); err != nil {
			t.Fatalf("GetWorkItem(%s): %v", id, err)
		}
	}

	stats, err := s.DBStatus(ctx, false)
	if err != nil {
		t.Fatalf("DBStatus: %v", err)
	}
	if stats.CacheHits+stats.CacheMisses == 0 {
		t.Fatalf("no cache activity recorded: %+v", stats)
	}
	if stats.HitRate() < 0 || stats.HitRate() > 1 {
		t.Fatalf("HitRate = %v, want [0,1]", stats.HitRate())
	}
	if stats.SchemaBytes <= 0 {
		t.Fatalf("SchemaBytes = %d, want > 0", stats.SchemaBytes)
	}

	// reset turns the cumulative counters into per-interval ones.
	if _, err := s.DBStatus(ctx, true); err != nil {
		t.Fatalf("DBStatus reset: %v", err)
	}
	after, err := s.DBStatus(ctx, false)
	if err != nil {
		t.Fatalf("DBStatus after reset: %v", err)
	}
	if after.CacheHits != 0 || after.CacheMisses != 0 {
		t.Fatalf("counters not reset: %+v", after)
	}
}
