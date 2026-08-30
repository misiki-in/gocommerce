//go:build !no_admin

// Package admin embeds the built admin panel into the binary.
//
// The panel is a SvelteKit single-page app built to ./build by `npm run build`.
// That directory is committed, so `go build` works on a fresh clone with no
// Node.js installed — which is what makes "one executable" true rather than
// "one executable, once you have a JavaScript toolchain".
//
// Build with `-tags no_admin` for an API-only binary.
package admin

import (
	"embed"
	"io/fs"
)

// The all: prefix matters: SvelteKit emits its assets into _app/, and embed
// skips underscore-prefixed directories without it.
//
//go:embed all:build
var buildDir embed.FS

// DistFS holds the built panel, rooted at the build directory itself.
var DistFS, _ = fs.Sub(buildDir, "build")
