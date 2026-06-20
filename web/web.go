// Package web embeds the server-rendered templates and static assets (htmx,
// Alpine, CSS) so the whole UI ships inside the single xdev binary. The embed
// directives must live in this directory because go:embed cannot reference
// parent paths.
package web

import "embed"

// TemplatesFS holds the html/template files under templates/.
//
//go:embed templates/*.html
var TemplatesFS embed.FS

// StaticFS holds CSS/JS assets served under /static/.
//
//go:embed static/*
var StaticFS embed.FS
