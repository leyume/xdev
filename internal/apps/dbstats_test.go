package apps

import "testing"

func TestHumanizeUptime(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, ""},     // unavailable renders as "—", not "0s"
		{-1, ""},    // a counter that failed to parse must not render as a duration
		{45, "45s"}, //
		{90, "1m"},  // seconds are noise once minutes exist
		{3600, "1h 0m"},
		{3661, "1h 1m"},
		{86400, "1d 0h"},
		{273600, "3d 4h"},
	}
	for _, tc := range cases {
		if got := humanizeUptime(tc.secs); got != tc.want {
			t.Errorf("humanizeUptime(%d) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

func TestHumanizeBytes(t *testing.T) {
	cases := []struct {
		b    float64
		want string
	}{
		{0, ""}, // renders as "—" on the page
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{134217728, "128.0 MiB"}, // the default InnoDB buffer pool
		{1073741824, "1.0 GiB"},
	}
	for _, tc := range cases {
		if got := humanizeBytes(tc.b); got != tc.want {
			t.Errorf("humanizeBytes(%v) = %q, want %q", tc.b, got, tc.want)
		}
	}
}

// The zero value has to be safe to render: SharedDBStats returns it whenever
// the server is down or unconfigured, and the page keys every field off
// Available rather than off an error.
func TestZeroStatsIsUnavailable(t *testing.T) {
	var st SharedDBStats
	if st.Available {
		t.Error("the zero SharedDBStats reports Available; a down server would render stale zeroes")
	}
	if st.Uptime != "" || st.PoolSize != "" || st.QPS != "" {
		t.Error("the zero SharedDBStats has non-empty fields; they must render as em-dashes")
	}
}
