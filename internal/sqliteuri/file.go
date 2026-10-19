// Package sqliteuri constructs file URIs for native SQLite paths.
package sqliteuri

import (
	"net/url"
	"path/filepath"
	"strings"
)

func File(path string) url.URL {
	name := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	return url.URL{Scheme: "file", Path: name}
}
