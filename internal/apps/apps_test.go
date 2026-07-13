package apps

import "testing"

// TestValidUpstream exercises the proxy-app upstream validator: bare
// http(s)://host[:port] passes (normalized); anything else is rejected.
func TestValidUpstream(t *testing.T) {
	good := map[string]string{
		"http://10.0.0.5:3000":      "http://10.0.0.5:3000",
		"https://coolify.example":   "https://coolify.example",
		" https://h.example:8443/ ": "https://h.example:8443", // trimmed, trailing slash dropped
	}
	for in, want := range good {
		got, err := validUpstream(in)
		if err != nil || got != want {
			t.Errorf("validUpstream(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "example.com", "ftp://x.example", "http://", "http://h.example/path",
		"http://h.example?x=1", "http://user:pw@h.example",
	} {
		if _, err := validUpstream(in); err == nil {
			t.Errorf("validUpstream(%q) should be rejected", in)
		}
	}
}
