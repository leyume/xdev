package server

import "testing"

// TestValidateEnvFile guards the lockout path: a line systemd's EnvironmentFile
// parser rejects stops xdev from starting, and this file is edited through the
// UI xdev itself serves — so a bad save has to be refused before it is written.
func TestValidateEnvFile(t *testing.T) {
	ok := []struct{ name, in string }{
		{"empty", ""},
		{"comments and blanks", "# a comment\n\n   \n# another\n"},
		{"plain pairs", "XDEV_ADDR=127.0.0.1:7331\nXDEV_SECURE=true\n"},
		{"empty value", "XDEV_ACME_EMAIL=\n"},
		{"value holds = and #", "XDEV_CMD=a=b # not a comment\n"},
		{"leading underscore key", "_PRIVATE=1\n"},
		{"indented pair", "  XDEV_ADDR=127.0.0.1:7331\n"},
	}
	for _, c := range ok {
		if err := validateEnvFile(c.in); err != nil {
			t.Errorf("%s: expected valid, got %v", c.name, err)
		}
	}

	bad := []struct{ name, in string }{
		{"bare word", "XDEV_ADDR\n"},
		{"key with dash", "XDEV-ADDR=1\n"},
		{"key starting with digit", "1FOO=bar\n"},
		{"shell syntax", "export XDEV_ADDR=1\n"},
		{"stray prose", "XDEV_ADDR=1\nthis is not a setting\n"},
		{"nul byte", "XDEV_ADDR=1\x00\n"},
	}
	for _, c := range bad {
		if err := validateEnvFile(c.in); err == nil {
			t.Errorf("%s: expected rejection, got nil", c.name)
		}
	}
}
