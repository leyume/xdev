package runtime

import "testing"

// TestSalient checks that the line we lift onto an engine error's first line is
// the one that explains the failure. The UI flashes only that first line, so
// "exit status 1" on its own used to be all a user ever saw.
func TestSalient(t *testing.T) {
	cases := []struct {
		name, out, want string
	}{
		{"empty", "", ""},
		{"empty lines only", "\n  \n", ""},
		{
			"prefers the error line over trailing noise",
			" Container demo_api_redis  Started\n" +
				"Error response from daemon: mkdir /var/www/html/storage: read-only file system\n",
			": Error response from daemon: mkdir /var/www/html/storage: read-only file system",
		},
		{
			"falls back to the last non-empty line",
			"Pulling app\nsomething happened\n\n",
			": something happened",
		},
		{"strips carriage returns", "\rbind: address already in use\n", ": bind: address already in use"},
	}
	for _, c := range cases {
		if got := salient(c.out); got != c.want {
			t.Errorf("%s: salient(%q) = %q, want %q", c.name, c.out, got, c.want)
		}
	}

	// Long lines are truncated so the flash message stays readable.
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'x'
	}
	if got := salient(string(long)); len(got) != 2+400+len("…") {
		t.Errorf("long line not truncated to 400 chars: len=%d", len(got))
	}
}
