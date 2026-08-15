package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"strconv"
	"strings"
)

// tmplFuncs are small formatting helpers available to all templates.
func tmplFuncs() template.FuncMap {
	return template.FuncMap{
		// mib converts a byte count to whole mebibytes.
		"mib": func(b int64) int64 { return b / 1024 / 1024 },
		// f1 formats a float with one decimal place.
		"f1": func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
		// gib formats a byte count as gibibytes with one decimal.
		"gib": func(b uint64) string { return fmt.Sprintf("%.1f", float64(b)/1073741824) },
		// hasPrefix drives active-nav-link highlighting from the request path.
		"hasPrefix": strings.HasPrefix,
		// trimPrefix shortens a URL for display (the link still carries the
		// scheme; "http://" in the visible text is noise beside a host:port).
		"trimPrefix": strings.TrimPrefix,
		// dict builds a map from alternating key/value pairs, for passing named
		// args to a shared sub-template ({{template "x" dict "K" v ...}}).
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, fmt.Errorf("dict: odd argument count")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict: key %d not a string", i)
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
		// json renders a value as JSON for a JavaScript context — Alpine's x-data
		// on the settings page seeds its rows from Go this way. HTML-escaping of
		// the attribute is transparent to the browser's JS parser.
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
		// initials renders a two-letter avatar badge from an email address.
		"initials": func(email string) string {
			local, _, _ := strings.Cut(email, "@")
			local = strings.ToUpper(local)
			if len(local) >= 2 {
				return local[:2]
			}
			return local
		},
	}
}
