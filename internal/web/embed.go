// Package web embeds the single-file web UI.
package web

import _ "embed"

//go:embed index.html
var IndexHTML []byte
