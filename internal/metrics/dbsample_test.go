package metrics

import (
	"path/filepath"
	"testing"
	"time"

	"xdev/internal/store"
)

func testCollector(t *testing.T) *Collector {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "xdev.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &Collector{store: st}
}

func TestSampleDBsRecordsNamedContainers(t *testing.T) {
	c := testCollector(t)
	now := time.Now()

	c.sampleDBs(now, []string{"xdev-db", "demo_app_db"}, map[string]sample{
		"xdev-db":     {cpu: 2.5, mem: 300 << 20},
		"demo_app_db": {cpu: 0.5, mem: 90 << 20},
		"demo_app":    {cpu: 9.9, mem: 500 << 20}, // an app, not a database
	})

	got, err := c.store.LatestDBMetrics(now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recorded %d containers, want 2: %v", len(got), got)
	}
	if got["xdev-db"].MemBytes != 300<<20 || got["xdev-db"].CPUPct != 2.5 {
		t.Errorf("xdev-db = %+v, want 300MiB / 2.5%%", got["xdev-db"])
	}
	if _, ok := got["demo_app"]; ok {
		t.Error("an application container was recorded as a database")
	}
}

// A database container that is stopped has no row in this tick's stats. It must
// leave a gap in the series, not a sample of zero — a flat zero line reads as
// "running and idle", which is the opposite of what is true.
func TestSampleDBsSkipsContainersWithNoStats(t *testing.T) {
	c := testCollector(t)
	now := time.Now()

	c.sampleDBs(now, []string{"xdev-db", "stopped_db"}, map[string]sample{
		"xdev-db": {cpu: 1, mem: 100 << 20},
	})

	got, err := c.store.LatestDBMetrics(now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if _, ok := got["stopped_db"]; ok {
		t.Error("a stopped database recorded a sample; its series should have a gap instead")
	}
	if len(got) != 1 {
		t.Fatalf("recorded %d containers, want 1", len(got))
	}
}

// A collector built without a DBSource (the nil case New documents) must not
// panic — it simply records no database samples.
func TestSampleDBsWithNoNamesIsANoop(t *testing.T) {
	c := testCollector(t)
	now := time.Now()

	c.sampleDBs(now, nil, map[string]sample{"xdev-db": {cpu: 1, mem: 1}})

	got, err := c.store.LatestDBMetrics(now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("recorded %d containers with no DBSource, want 0", len(got))
	}
}
