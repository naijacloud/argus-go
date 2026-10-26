package argus

import (
	"encoding/json"
	"time"
)

// Level is an event's severity.
type Level string

// The levels Argus reads (spec §4).
const (
	LevelFatal   Level = "fatal"
	LevelError   Level = "error"
	LevelWarning Level = "warning"
	LevelInfo    Level = "info"
	LevelDebug   Level = "debug"
)

// Event is one error or message as Argus receives it. BeforeSend sees it
// before it is scrubbed and sent.
type Event struct {
	EventID     string            `json:"event_id"`
	Timestamp   time.Time         `json:"timestamp"`
	Level       Level             `json:"level"`
	Platform    string            `json:"platform"`
	Release     string            `json:"release,omitempty"`
	Environment string            `json:"environment,omitempty"`
	ServerName  string            `json:"server_name,omitempty"`
	Transaction string            `json:"transaction,omitempty"`
	Message     string            `json:"message,omitempty"`
	Fingerprint []string          `json:"fingerprint,omitempty"`
	Tags        map[string]string `json:"tags,omitempty"`
	User        *User             `json:"user,omitempty"`
	Contexts    map[string]any    `json:"contexts,omitempty"`
	Extra       map[string]any    `json:"extra,omitempty"`
	Request     *Request          `json:"request,omitempty"`
	SDK         SDKInfo           `json:"sdk"`

	// Exception is the error, last, after its causes (deepest first).
	Exception []Exception `json:"-"`
	// Breadcrumbs are oldest first.
	Breadcrumbs []Breadcrumb `json:"-"`
}

// MarshalJSON writes exception and breadcrumbs in Sentry's {"values": [...]} form.
func (e *Event) MarshalJSON() ([]byte, error) {
	type alias Event
	w := struct {
		*alias
		Exception   *values[Exception]  `json:"exception,omitempty"`
		Breadcrumbs *values[Breadcrumb] `json:"breadcrumbs,omitempty"`
	}{alias: (*alias)(e)}
	if len(e.Exception) > 0 {
		w.Exception = &values[Exception]{e.Exception}
	}
	if len(e.Breadcrumbs) > 0 {
		w.Breadcrumbs = &values[Breadcrumb]{e.Breadcrumbs}
	}
	return json.Marshal(w)
}

type values[T any] struct {
	Values []T `json:"values"`
}

// SDKInfo names this SDK on every event.
type SDKInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Exception is one error in a chain.
type Exception struct {
	Type       string      `json:"type"`
	Value      string      `json:"value"`
	Module     string      `json:"module,omitempty"`
	Stacktrace *Stacktrace `json:"stacktrace,omitempty"`
}

// Stacktrace holds frames oldest first.
type Stacktrace struct {
	Frames []Frame `json:"frames"`
}

// Frame is one stack frame.
type Frame struct {
	Filename    string   `json:"filename,omitempty"`
	AbsPath     string   `json:"abs_path,omitempty"`
	Function    string   `json:"function,omitempty"`
	Module      string   `json:"module,omitempty"`
	Lineno      int      `json:"lineno,omitempty"`
	InApp       bool     `json:"in_app"`
	PreContext  []string `json:"pre_context,omitempty"`
	ContextLine string   `json:"context_line,omitempty"`
	PostContext []string `json:"post_context,omitempty"`
}

// User identifies who hit an error. IPAddress is only sent with SendDefaultPii.
type User struct {
	ID        string `json:"id,omitempty"`
	Email     string `json:"email,omitempty"`
	Username  string `json:"username,omitempty"`
	IPAddress string `json:"ip_address,omitempty"`
}

// Breadcrumb is something that happened before an error.
type Breadcrumb struct {
	Timestamp time.Time      `json:"timestamp"`
	Type      string         `json:"type,omitempty"`
	Category  string         `json:"category,omitempty"`
	Message   string         `json:"message,omitempty"`
	Level     Level          `json:"level,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// Request describes the HTTP request an error happened in. Bodies and
// cookies are never sent, and the URL carries no query string.
type Request struct {
	Method  string            `json:"method,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}
