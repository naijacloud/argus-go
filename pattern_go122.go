//go:build !go1.23

package argus

import "net/http"

// requestPattern: Go 1.22's ServeMux does not expose the matched pattern.
func requestPattern(*http.Request) string { return "" }

const patternSupported = false
