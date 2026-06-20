// Package naming turns human names ("My Cool Site") into URL/DNS/container-safe
// slugs ("my-cool-site") and resolves collisions by suffixing -2, -3, ...
package naming

import "strings"

// Slugify lowercases, keeps [a-z0-9], and collapses everything else to single
// dashes. Always returns a non-empty result.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "app"
	}
	return out
}

// Unique returns Slugify(name), or name-2, name-3, ... until taken(candidate)
// reports false. taken should return true when a slug already exists.
func Unique(name string, taken func(string) bool) string {
	base := Slugify(name)
	if !taken(base) {
		return base
	}
	for i := 2; ; i++ {
		candidate := base + "-" + itoa(i)
		if !taken(candidate) {
			return candidate
		}
	}
}

// itoa is a tiny non-allocating-ish integer formatter (avoids importing strconv
// for one call site and keeps intent obvious).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
