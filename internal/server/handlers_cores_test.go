package server

import (
	"strings"
	"testing"

	"xdev/internal/metrics"
	"xdev/internal/runtime"
	"xdev/internal/store"
)

// Apps add up in cores (each sample's 100% is one core) and as a share of
// the host, never more than the host's own total.
func TestSumAppUse(t *testing.T) {
	h := metrics.Host{CPUPct: 40, Cores: 2, MemUsed: 4 << 30, MemTotal: 8 << 30}
	rows := []appRow{
		{HasMetrics: true, CPUPct: 30, MemMiB: 1024},
		{HasMetrics: true, CPUPct: 20, MemMiB: 512},
		{HasMetrics: false, CPUPct: 99, MemMiB: 9999}, // no recent sample: not counted
	}
	u := sumAppUse(rows, h)
	if u.Sampled != 2 || u.Cores != 0.5 || u.MemBytes != 1536<<20 {
		t.Fatalf("use = %+v", u)
	}
	if u.CPUShare != 25 || u.OtherCPU != 15 {
		t.Errorf("cpu share %v other %v, want 25 and 15", u.CPUShare, u.OtherCPU)
	}
	if u.MemShare != 18.75 || u.OtherMem != 31.25 {
		t.Errorf("mem share %v other %v, want 18.75 and 31.25", u.MemShare, u.OtherMem)
	}
	// A sample can run ahead of the host's: apps never exceed the total.
	if u := sumAppUse([]appRow{{HasMetrics: true, CPUPct: 190}}, h); u.CPUShare != 40 || u.OtherCPU != 0 {
		t.Errorf("capped share = %+v", u)
	}
}

// The host card reads CPU in cores with the apps' part beside it.
func TestHostResourcesShowCores(t *testing.T) {
	h := metrics.Host{CPUPct: 40, Cores: 2, MemUsed: 4 << 30, MemTotal: 8 << 30, Hostname: "srv"}
	rows := []appRow{{App: store.App{ID: 1, Name: "web", Type: "static", Status: store.AppRunning}, HasMetrics: true, CPUPct: 30, MemMiB: 1024}}
	out := renderNamed(t, "dashboard", viewData{
		"Host": h, "AppsUse": sumAppUse(rows, h), "Apps": rows, "Projects": []store.Project{},
		"Events": []store.Event{}, "Runtime": runtime.Info{}, "Engine": "docker",
		"TotalApps": 1, "Running": 1, "Stopped": 0,
	})
	for _, want := range []string{"0.80 / 2 cores · 40.0%", "Apps 0.30", "Other 0.50", "Apps 1.0 GB", "Other 3.0 GB", `class="seg-apps"`, "CPU (cores)", "% of 2 cores"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
}
