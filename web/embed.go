// Package web embeds the static frontend so the server is a single binary.
package web

import "embed"

//go:embed index.html manifest.webmanifest static vendor
var Files embed.FS
