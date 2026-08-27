// Package web embeds the browser UI.
package web

import (
	"embed"
	"io/fs"
)

//go:embed index.html
var files embed.FS

// FS is the static asset filesystem the server mounts at /.
var FS fs.FS = files
