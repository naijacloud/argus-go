//go:build go1.23

package argus

import "net/http"

// requestPattern is the ServeMux pattern that matched (Go 1.23+).
func requestPattern(r *http.Request) string { return r.Pattern }

const patternSupported = true
