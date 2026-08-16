package metrics

import "testing"

// byPrefix mirrors what store.AppPrefixes yields: "<project-slug>_<app-slug>".
func byPrefix() map[string]int64 {
	return map[string]int64{
		"rm_folu":      15,
		"rm_piano":     19,
		"servorien_l2": 49,
		"bizev_bizev":  23,
		"barja_barja":  20,
		"servorien_s2": 41,
	}
}

// The bug this fixes: an imported compose file names its own containers, and
// those names have nothing to do with the app slug. Attribution by name prefix
// missed every one of them, so only xdev-generated stacks reported metrics.
func TestComposeProjectLabelAttributesUserNamedContainers(t *testing.T) {
	cases := []struct {
		name, project string
		want          int64
	}{
		{"foluolatona", "rm_folu", 15},
		{"pianocrib", "rm_piano", 19},
		{"bizev", "bizev_bizev", 23},
		{"barja", "barja_barja", 20},
	}
	for _, tc := range cases {
		got, ok := attribute(tc.name, tc.project, byPrefix())
		if !ok {
			t.Fatalf("container %q (project %q) was not attributed to any app", tc.name, tc.project)
		}
		if got != tc.want {
			t.Errorf("container %q: attributed to app %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Without the label these names are unreachable — the assertion that the old
// code path genuinely could not see them, so the fallback is not enough on its
// own and the label lookup has to come first.
func TestUserNamedContainersAreInvisibleToPrefixMatching(t *testing.T) {
	for _, name := range []string{"foluolatona", "pianocrib", "bizev", "barja"} {
		if id, ok := attribute(name, "", byPrefix()); ok {
			t.Errorf("container %q matched app %d by name alone; the test's premise is wrong", name, id)
		}
	}
}

// Generated stacks are named "<prefix>_<service>" and must keep working when
// the engine reports no labels at all (older engine, or `ps` failing while
// `stats` succeeds).
func TestNamePrefixStillAttributesGeneratedStacks(t *testing.T) {
	for _, name := range []string{"servorien_l2_app", "servorien_l2_redis", "servorien_l2_adminer"} {
		got, ok := attribute(name, "", byPrefix())
		if !ok {
			t.Fatalf("generated container %q lost its attribution", name)
		}
		if got != 49 {
			t.Errorf("container %q: attributed to app %d, want 49", name, got)
		}
	}
}

// A compose project xdev does not know about must not be charged to an app
// whose slug happens to be a prefix of the container's name.
func TestUnknownProjectIsNotAttributed(t *testing.T) {
	if id, ok := attribute("mailcow-redis-1", "mailcowdockerized", byPrefix()); ok {
		t.Errorf("foreign container attributed to app %d", id)
	}
}

// An app whose slug is a prefix of another's ("servorien_s2" vs a container
// named "servorien_s2x_web") must not swallow its neighbour: the separator is
// part of the match.
func TestPrefixMatchRequiresTheSeparator(t *testing.T) {
	if id, ok := attribute("servorien_s2x", "", byPrefix()); ok {
		t.Errorf("container %q attributed to app %d; prefix match ignored the separator", "servorien_s2x", id)
	}
}

// The label wins over the name when both would resolve, so a container that
// compose moved between projects is charged to the project it now belongs to.
func TestLabelWinsOverName(t *testing.T) {
	got, ok := attribute("servorien_l2_app", "rm_folu", byPrefix())
	if !ok || got != 15 {
		t.Errorf("attributed to %d (ok=%v), want 15 from the label", got, ok)
	}
}

func TestParseProjects(t *testing.T) {
	out := "foluolatona\trm_folu\n" +
		"servorien_l2_app\tservorien_l2\n" +
		"xdev-db\t\n" + // started outside compose: no label
		"weird-line-without-a-tab\n" +
		"adminer\t<no value>\n" // engine's rendering of a missing label

	got := parseProjects(out)
	want := map[string]string{
		"foluolatona":      "rm_folu",
		"servorien_l2_app": "servorien_l2",
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries (%v), want %d", len(got), got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q -> %q, want %q", k, got[k], v)
		}
	}
}

// Overlapping prefixes must resolve to the more specific app, and must do so
// every time — the fallback scans a map, whose order is randomised per run.
func TestLongestPrefixWinsDeterministically(t *testing.T) {
	prefixes := map[string]int64{"servorien": 7, "servorien_l2": 49}
	for i := 0; i < 200; i++ {
		got, ok := attribute("servorien_l2_app", "", prefixes)
		if !ok || got != 49 {
			t.Fatalf("iteration %d: attributed to %d (ok=%v), want 49", i, got, ok)
		}
	}
}
