// Package web embeds the static files for the web UI.
package web

import "embed"

//go:embed static
var StaticFiles embed.FS
