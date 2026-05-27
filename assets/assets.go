// Package assets embeds static files compiled into the binary.
package assets

import "embed"

//go:embed icon.ico
var IconICO []byte

//go:embed icon.png
var IconPNG []byte

//go:embed templates/*.html
var TemplateFS embed.FS

//go:embed static/*
var StaticFS embed.FS
