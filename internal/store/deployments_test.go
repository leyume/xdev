package store

import "testing"

// TestDeploymentSteps pins how a deploy log becomes the list the settings page
// renders. The log format is written by the deploy itself ("$ <command>" then
// that command's output), so the two have to agree.
func TestDeploymentSteps(t *testing.T) {
	d := Deployment{Status: DeployFailed, Log: `$ composer install
Installing dependencies
86 installs

$ php artisan migrate --force
SQLSTATE[42S01]: table already exists`}

	steps := d.Steps()
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2: %+v", len(steps), steps)
	}
	if steps[0].Name != "composer install" || steps[0].Failed {
		t.Errorf("first step = %+v, want the composer step, not failed", steps[0])
	}
	if !contains(steps[0].Output, "86 installs") {
		t.Errorf("first step lost its output: %q", steps[0].Output)
	}
	// A deploy stops at its first failure, so the last step is the one that broke.
	if !steps[1].Failed {
		t.Error("the step a failed deploy stopped on is not marked failed")
	}

	// A success marks nothing failed.
	ok := Deployment{Status: DeployOK, Log: "$ a\nout\n$ b\nout"}
	for i, st := range ok.Steps() {
		if st.Failed {
			t.Errorf("step %d of a successful deploy marked failed", i)
		}
	}

	// A host app's build log has no markers at all: one unnamed step, so it
	// renders exactly as it did before steps existed.
	plain := Deployment{Status: DeployOK, Log: "npm ERR! something\nmore"}
	ps := plain.Steps()
	if len(ps) != 1 || ps[0].Name != "" || !contains(ps[0].Output, "npm ERR!") {
		t.Errorf("unmarked log = %+v, want one unnamed step holding all of it", ps)
	}

	// Nothing to show is nothing to render.
	if got := (Deployment{Log: "   "}).Steps(); got != nil {
		t.Errorf("blank log produced %d steps", len(got))
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (hay == needle ||
		len(needle) == 0 || indexOf(hay, needle) >= 0)
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
