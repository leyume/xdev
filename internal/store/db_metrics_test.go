package store

import (
	"testing"
	"time"
)

func TestLatestDBMetricsTakesTheNewestPerContainer(t *testing.T) {
	st := testStore(t)
	base := time.Now().Add(-time.Minute)

	// Two containers, three samples each, deliberately inserted out of order so
	// the query cannot pass by accident of insertion order.
	for _, s := range []struct {
		container string
		offset    time.Duration
		mem       int64
	}{
		{"xdev-db", 30 * time.Second, 200},
		{"xdev-db", 10 * time.Second, 100},
		{"xdev-db", 50 * time.Second, 300}, // newest
		{"demo_app_db", 20 * time.Second, 40},
		{"demo_app_db", 40 * time.Second, 60}, // newest
	} {
		if err := st.InsertDBMetric(s.container, base.Add(s.offset), 1.5, s.mem); err != nil {
			t.Fatalf("insert %s: %v", s.container, err)
		}
	}

	got, err := st.LatestDBMetrics(base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d containers, want 2: %v", len(got), got)
	}
	if got["xdev-db"].MemBytes != 300 {
		t.Errorf("xdev-db mem = %d, want 300 (the newest sample)", got["xdev-db"].MemBytes)
	}
	if got["demo_app_db"].MemBytes != 60 {
		t.Errorf("demo_app_db mem = %d, want 60 (the newest sample)", got["demo_app_db"].MemBytes)
	}
}

// The dashboard and DB page ask for a short window so a stopped database shows
// nothing rather than its last-known figure. Samples outside that window must
// not come back at all.
func TestLatestDBMetricsHonoursTheWindow(t *testing.T) {
	st := testStore(t)
	now := time.Now()

	if err := st.InsertDBMetric("stale-db", now.Add(-10*time.Minute), 1, 999); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.InsertDBMetric("live-db", now, 1, 111); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := st.LatestDBMetrics(now.Add(-20 * time.Second))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if _, ok := got["stale-db"]; ok {
		t.Error("a sample older than the window was returned; a stopped database would show a stale figure")
	}
	if got["live-db"].MemBytes != 111 {
		t.Errorf("live-db mem = %d, want 111", got["live-db"].MemBytes)
	}
}

func TestRecentDBMetricsIsScopedAndOrdered(t *testing.T) {
	st := testStore(t)
	base := time.Now().Add(-time.Minute)

	for i, mem := range []int64{10, 20, 30} {
		if err := st.InsertDBMetric("xdev-db", base.Add(time.Duration(i)*time.Second), 0, mem); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if err := st.InsertDBMetric("other-db", base, 0, 999); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := st.RecentDBMetrics("xdev-db", base.Add(-time.Hour))
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d samples, want 3 (other-db must not leak in)", len(got))
	}
	for i, want := range []int64{10, 20, 30} {
		if got[i].MemBytes != want {
			t.Errorf("sample %d = %d, want %d — series is not oldest-first", i, got[i].MemBytes, want)
		}
	}
}

func TestPruneDBMetricsBefore(t *testing.T) {
	st := testStore(t)
	now := time.Now()

	if err := st.InsertDBMetric("xdev-db", now.Add(-48*time.Hour), 0, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.InsertDBMetric("xdev-db", now, 0, 2); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.PruneDBMetricsBefore(now.Add(-24 * time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	got, err := st.RecentDBMetrics("xdev-db", now.Add(-72*time.Hour))
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(got) != 1 || got[0].MemBytes != 2 {
		t.Errorf("after prune got %v, want only the recent sample", got)
	}
}
