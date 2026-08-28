//go:build !embedui

// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import "io/fs"

// FS returns nil: the UI isn't embedded in this build. Run `make web-dev` for the
// dev server, or build with -tags embedui to embed the production UI.
func FS() fs.FS { return nil }
