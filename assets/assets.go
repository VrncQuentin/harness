// Package assets embeds static files compiled into the binary.
package assets

import "embed"

//go:embed icon.png
var IconPNG []byte

//go:embed templates/status.html
var TemplateFS embed.FS
