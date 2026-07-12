package metrics

import "github.com/shirou/gopsutil/v3/process"

// ProcSource yields the leader pid of each running host-process (static) app,
// keyed by app id. Satisfied by hostproc.Supervisor; kept as an interface here
// so the metrics package doesn't import hostproc.
type ProcSource interface {
	PIDs() map[int64]int
}

// procTotals sums cumulative CPU seconds (user+system) and resident memory
// across a process and its descendants — a static app is an `npm`→`node` tree,
// so the leader alone under-counts. CPU% is derived by the collector from the
// delta of cpuSecs between ticks; rss is instantaneous.
func procTotals(pid int) (cpuSecs float64, rss uint64) {
	seen := map[int32]bool{}
	var walk func(p *process.Process, depth int)
	walk = func(p *process.Process, depth int) {
		if p == nil || depth > 8 || seen[p.Pid] {
			return
		}
		seen[p.Pid] = true
		if t, err := p.Times(); err == nil {
			cpuSecs += t.User + t.System
		}
		if m, err := p.MemoryInfo(); err == nil && m != nil {
			rss += m.RSS
		}
		kids, _ := p.Children()
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	if p, err := process.NewProcess(int32(pid)); err == nil {
		walk(p, 0)
	}
	return
}
