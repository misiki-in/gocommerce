//go:build no_admin

package admin

import "io/fs"

// DistFS is deliberately nil so the panel is not bundled into the binary.
// The engine checks for nil and simply serves no /_/ routes.
var DistFS fs.FS
