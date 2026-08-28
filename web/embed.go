//go:build embedui

// SPDX-License-Identifier: AGPL-3.0-or-later

// Package web embeds the built single-page app so the control-plane can serve it
// from the same origin as the API. Only compiled with -tags embedui (release
// builds); dev builds use the stub in embed_stub.go and run `make web-dev`.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the built UI rooted at dist/, or nil if it can't be read.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	return sub
}
