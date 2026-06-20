package server

import (
	"fmt"
	"html/template"
	"strconv"
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
	}
}
