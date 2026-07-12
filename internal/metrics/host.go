package metrics

import (
	"fmt"
	"os"
	"time"

	"github.com/shirou/gopsutil/v3/cpu"
	"github.com/shirou/gopsutil/v3/disk"
	"github.com/shirou/gopsutil/v3/host"
	"github.com/shirou/gopsutil/v3/mem"
)

// Host is a point-in-time snapshot of overall machine resource usage.
type Host struct {
	CPUPct        float64
	MemUsed       uint64
	MemTotal      uint64
	DiskUsed      uint64
	DiskTotal     uint64
	Hostname      string
	UptimeSeconds uint64
}

// MemPct / DiskPct are convenience helpers for templates.
func (h Host) MemPct() float64  { return pct(h.MemUsed, h.MemTotal) }
func (h Host) DiskPct() float64 { return pct(h.DiskUsed, h.DiskTotal) }

// Uptime formats UptimeSeconds as a short duration string (e.g. "42d", "3h").
func (h Host) Uptime() string {
	d := time.Duration(h.UptimeSeconds) * time.Second
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

// HostSnapshot reads current host CPU/memory/disk usage. CPU is measured over a
// short window, so this call blocks briefly (~200ms).
func HostSnapshot() Host {
	var h Host
	if pcts, err := cpu.Percent(200*time.Millisecond, false); err == nil && len(pcts) > 0 {
		h.CPUPct = pcts[0]
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		h.MemUsed = vm.Used
		h.MemTotal = vm.Total
	}
	if du, err := disk.Usage("/"); err == nil {
		h.DiskUsed = du.Used
		h.DiskTotal = du.Total
	}
	h.Hostname, _ = os.Hostname()
	if up, err := host.Uptime(); err == nil {
		h.UptimeSeconds = up
	}
	return h
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}
