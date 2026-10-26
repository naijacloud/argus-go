package argus

import (
	"crypto/rand"
	"encoding/hex"
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newEventID() string { return randomHex(16) }
func newTraceID() string { return randomHex(16) }
func newSpanID() string  { return randomHex(8) }
