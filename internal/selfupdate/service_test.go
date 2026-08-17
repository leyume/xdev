package selfupdate

import (
	"context"
	"testing"
	"time"
)

// An unmanaged service is not a failure — plenty of installs run xdev from a
// terminal — but it must never claim a restart happened.
func TestUnmanagedServiceDoesNothing(t *testing.T) {
	var s Service
	if s.Managed() {
		t.Error("the zero Service reports itself managed")
	}
	if s.String() != "none" {
		t.Errorf("String() = %q, want none", s.String())
	}
	if err := s.Restart(context.Background()); err != nil {
		t.Errorf("Restart on an unmanaged service: %v", err)
	}
	if s.Active(context.Background()) {
		t.Error("an unmanaged service reported active")
	}
	if s.MainPID(context.Background()) != 0 {
		t.Error("an unmanaged service reported a pid")
	}
	if s.WaitRestarted(context.Background(), time.Second, 0) {
		t.Error("an unmanaged service reported a successful restart")
	}
}

// The regression this guards: a restart that fails outright leaves the old
// process running, every liveness check passes, and an updater that only asks
// "is it active?" reports success while the machine serves the old binary. The
// pid is what distinguishes the two.
func TestParseLaunchdPID(t *testing.T) {
	out := `system/com.leyume.xdev = {
	active count = 1
	path = /Library/LaunchDaemons/com.leyume.xdev.plist
	state = running
	program = /usr/local/bin/xdev
	pid = 4821
	immediate reason = speculative
}`
	if got := parseLaunchdPID(out); got != 4821 {
		t.Errorf("parseLaunchdPID = %d, want 4821", got)
	}

	// A stopped job prints no pid at all; 0 means "not running", and must not
	// be confused with a real process.
	stopped := "system/com.leyume.xdev = {\n\tstate = not running\n}"
	if got := parseLaunchdPID(stopped); got != 0 {
		t.Errorf("parseLaunchdPID on a stopped job = %d, want 0", got)
	}

	// "pid" must match the whole key, not appear inside another one.
	noisy := "\tlast exit pid = 999\n\tpid = 12\n"
	if got := parseLaunchdPID(noisy); got != 12 {
		t.Errorf("parseLaunchdPID picked up the wrong key: %d, want 12", got)
	}
}

// DetectService must not claim systemd just because systemctl exists — most
// Linux machines have it and only some run xdev under it. It is allowed to find
// a real xdev unit on a machine that has one, so this asserts the shape of the
// answer rather than a fixed value.
func TestDetectServiceIsHonestAboutWhatItFound(t *testing.T) {
	s := DetectService(context.Background())
	switch s.Kind {
	case "":
		if s.Managed() {
			t.Error("an empty Kind reported as managed")
		}
	case "systemd":
		if s.Name != systemdUnit {
			t.Errorf("systemd service name = %q, want %q", s.Name, systemdUnit)
		}
	case "launchd":
		if s.Name != launchdLabel {
			t.Errorf("launchd service name = %q, want %q", s.Name, launchdLabel)
		}
	default:
		t.Errorf("unknown service kind %q", s.Kind)
	}
}
