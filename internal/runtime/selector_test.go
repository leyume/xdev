package runtime

import "testing"

func TestCountLines(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"\n\n", 0},
		{"abc123", 1},
		{"abc\ndef\n", 2},
		{"  \nabc\n\ndef\n  ", 2},
	}
	for _, c := range cases {
		if got := countLines([]byte(c.in)); got != c.want {
			t.Errorf("countLines(%q): got %d, want %d", c.in, got, c.want)
		}
	}
}
