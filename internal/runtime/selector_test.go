package runtime

import "testing"

func TestPickDefault(t *testing.T) {
	st := func(e Engine, ready bool) EngineStatus {
		return EngineStatus{Engine: e, Installed: true, ComposeOK: true, Ready: ready}
	}
	cases := []struct {
		name    string
		engines []EngineStatus
		goos    string
		want    Engine
	}{
		{"mac prefers kiri", []EngineStatus{st(Kiri, true), st(Podman, true), st(Docker, true)}, "darwin", Kiri},
		{"mac: a running podman beats a stopped kiri", []EngineStatus{st(Kiri, false), st(Podman, true)}, "darwin", Podman},
		{"mac: nothing running, kiri installed", []EngineStatus{st(Kiri, false), st(Podman, false)}, "darwin", Kiri},
		{"linux prefers docker", []EngineStatus{st(Kiri, true), st(Docker, true)}, "linux", Docker},
		{"nothing installed on mac", nil, "darwin", Podman},
	}
	for _, c := range cases {
		if got := pickDefault(Info{Engines: c.engines}, c.goos); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
	if Known("lxc") || !Known(Kiri) {
		t.Error("Known")
	}
}
