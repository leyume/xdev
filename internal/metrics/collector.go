// Package metrics samples per-app container resource usage on an interval and
// stores it as a time series in sqlite, plus exposes a host-level snapshot for
// the dashboard. It shells out to `<engine> stats` (the same engine xdev uses)
// and attributes each container to an app by its name prefix.
package metrics

import (
	"context"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"xdev/internal/runtime"
	"xdev/internal/store"
)

const (
	interval  = 10 * time.Second
	retention = 24 * time.Hour
)

// Collector periodically samples container stats into the store.
type Collector struct {
	store *store.Store
	sel   *runtime.Selector
}

// New creates a Collector that samples whichever engines are usable.
func New(st *store.Store, sel *runtime.Selector) *Collector {
	return &Collector{store: st, sel: sel}
}

// Run loops until ctx is cancelled, sampling every interval.
func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	c.collectOnce(ctx) // sample immediately on start
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collectOnce(ctx)
		}
	}
}

// collectOnce reads `stats` once, aggregates per app, and stores a row each.
func (c *Collector) collectOnce(ctx context.Context) {
	prefixes, err := c.store.AppPrefixes()
	if err != nil || len(prefixes) == 0 {
		return
	}

	samples, err := c.readStats(ctx)
	if err != nil {
		return // engine may be momentarily unavailable; try again next tick
	}

	type agg struct {
		cpu float64
		mem int64
	}
	byApp := map[int64]agg{}
	for name, s := range samples {
		for _, p := range prefixes {
			if strings.HasPrefix(name, p.Prefix+"_") {
				a := byApp[p.ID]
				a.cpu += s.cpu
				a.mem += s.mem
				byApp[p.ID] = a
				break
			}
		}
	}

	now := time.Now()
	limitByID := map[int64]int64{}
	for _, p := range prefixes {
		limitByID[p.ID] = p.MemLimit
	}
	for id, a := range byApp {
		if err := c.store.InsertMetric(id, now, a.cpu, a.mem, limitByID[id]); err != nil {
			log.Printf("metrics insert: %v", err)
		}
	}
	c.store.PruneMetricsBefore(now.Add(-retention))
}

type sample struct {
	cpu float64
	mem int64
}

// readStats samples every usable engine and merges the results, so apps run on
// different engines are all covered. An engine whose daemon is down just yields
// an error and is skipped.
func (c *Collector) readStats(ctx context.Context) (map[string]sample, error) {
	res := map[string]sample{}
	var lastErr error
	for _, eng := range c.sel.UsableEngines() {
		m, err := statsForEngine(ctx, eng)
		if err != nil {
			lastErr = err
			continue
		}
		for k, v := range m {
			res[k] = v
		}
	}
	if len(res) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return res, nil
}

// statsForEngine runs `<engine> stats --no-stream` with a stable format and
// parses container name, CPU%, and memory-in-use.
func statsForEngine(ctx context.Context, engine runtime.Engine) (map[string]sample, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, string(engine),
		"stats", "--no-stream", "--format", "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	res := map[string]sample{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		cpu := parsePercent(parts[1])
		mem := parseMemUsage(parts[2]) // "10.5MiB / 1.9GiB" -> first value
		res[name] = sample{cpu: cpu, mem: mem}
	}
	return res, nil
}

// parsePercent turns "0.50%" into 0.50.
func parsePercent(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// parseMemUsage takes "10.5MiB / 1.9GiB" and returns the used bytes.
func parseMemUsage(s string) int64 {
	used := s
	if i := strings.IndexByte(s, '/'); i >= 0 {
		used = s[:i]
	}
	return parseSize(strings.TrimSpace(used))
}

// parseSize parses a human size like "10.5MiB", "512MB", "1.2GB", "900kB", "42B".
func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "--" {
		return 0
	}
	// Split number from unit.
	i := 0
	for i < len(s) && (s[i] == '.' || (s[i] >= '0' && s[i] <= '9')) {
		i++
	}
	num, _ := strconv.ParseFloat(s[:i], 64)
	unit := strings.ToLower(strings.TrimSpace(s[i:]))

	var mult float64 = 1
	switch {
	case strings.HasPrefix(unit, "k"):
		mult = 1024
	case strings.HasPrefix(unit, "m"):
		mult = 1024 * 1024
	case strings.HasPrefix(unit, "g"):
		mult = 1024 * 1024 * 1024
	case strings.HasPrefix(unit, "t"):
		mult = 1024 * 1024 * 1024 * 1024
	}
	return int64(num * mult)
}
