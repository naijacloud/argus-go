package argus

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"
)

// Details adds to one captured event. Every field is optional.
type Details struct {
	Level       Level
	Tags        map[string]string
	Extra       map[string]any
	User        *User
	Fingerprint []string
	// Transaction is the route or job; it defaults to the active span's name.
	Transaction string
	Request     *Request
}

// CaptureException sends err, its cause chain and the stack where it was
// captured. It returns the event id, or "" when nothing was sent (no DSN,
// sampled out, dropped by BeforeSend, or a nil err).
//
// Errors made by github.com/pkg/errors keep their own stack.
func CaptureException(ctx context.Context, err error, details ...Details) string {
	c := get()
	if c == nil || err == nil {
		return ""
	}
	defer c.guard()
	e := &Event{Exception: exceptionChain(err, callerStack())}
	return c.capture(ctx, e, merge(details))
}

// CaptureMessage sends a message. An empty level means info.
func CaptureMessage(ctx context.Context, message string, level Level, details ...Details) string {
	c := get()
	if c == nil {
		return ""
	}
	defer c.guard()
	d := merge(details)
	if d.Level == "" {
		d.Level = level
	}
	if d.Level == "" {
		d.Level = LevelInfo
	}
	return c.capture(ctx, &Event{Message: message}, d)
}

func merge(ds []Details) Details {
	var out Details
	for _, d := range ds {
		if d.Level != "" {
			out.Level = d.Level
		}
		if d.Tags != nil {
			if out.Tags == nil {
				out.Tags = map[string]string{}
			}
			for k, v := range d.Tags {
				out.Tags[k] = v
			}
		}
		if d.Extra != nil {
			if out.Extra == nil {
				out.Extra = map[string]any{}
			}
			for k, v := range d.Extra {
				out.Extra[k] = v
			}
		}
		if d.User != nil {
			out.User = d.User
		}
		if d.Fingerprint != nil {
			out.Fingerprint = d.Fingerprint
		}
		if d.Transaction != "" {
			out.Transaction = d.Transaction
		}
		if d.Request != nil {
			out.Request = d.Request
		}
	}
	return out
}

// capture fills, filters, scrubs and queues an event.
func (c *client) capture(ctx context.Context, e *Event, d Details) string {
	ctx = orBackground(ctx)
	if !c.sample(c.opts.SampleRate) {
		return ""
	}
	e.EventID = newEventID()
	e.Timestamp = time.Now().UTC()
	e.Platform = "go"
	e.Release = c.opts.Release
	e.Environment = c.opts.Environment
	e.ServerName = c.opts.ServerName
	e.SDK = SDKInfo{Name: sdkName, Version: Version}
	e.Level = LevelError
	if d.Level != "" {
		e.Level = d.Level
	}
	applyScopes(ctx, e, c.opts.MaxBreadcrumbs)
	if e.Contexts == nil {
		e.Contexts = map[string]any{}
	}
	for k, v := range runtimeContexts() {
		if _, set := e.Contexts[k]; !set {
			e.Contexts[k] = v
		}
	}
	if span := SpanFromContext(ctx); span != nil {
		e.Transaction = span.transaction()
		e.Contexts["trace"] = map[string]any{"trace_id": span.traceID, "span_id": span.spanID}
	} else if rp, ok := ctx.Value(remoteParentKey{}).(spanContext); ok {
		e.Contexts["trace"] = map[string]any{"trace_id": rp.traceID, "span_id": rp.spanID}
	}
	for k, v := range d.Tags {
		if e.Tags == nil {
			e.Tags = map[string]string{}
		}
		e.Tags[k] = v
	}
	for k, v := range d.Extra {
		if e.Extra == nil {
			e.Extra = map[string]any{}
		}
		e.Extra[k] = v
	}
	if d.User != nil {
		u := *d.User
		e.User = &u
	}
	if d.Fingerprint != nil {
		e.Fingerprint = d.Fingerprint
	}
	if d.Transaction != "" {
		e.Transaction = d.Transaction
	}
	if d.Request != nil {
		e.Request = d.Request
	}
	if e.Request != nil {
		r := *e.Request
		r.URL = stripQuery(r.URL)
		e.Request = &r
	}
	if e.User != nil && !c.opts.SendDefaultPii {
		e.User.IPAddress = ""
	}
	if c.opts.BeforeSend != nil {
		if e = c.callBeforeSend(e); e == nil {
			c.debugf("errors: BeforeSend dropped the event")
			return ""
		}
	}
	truncateEvent(e)
	jsonSafe(e.Extra)
	jsonSafe(e.Contexts)
	for i := range e.Breadcrumbs {
		jsonSafe(e.Breadcrumbs[i].Data)
	}
	payload, err := scrubEvent(e)
	if err != nil {
		c.debugf("errors: the event could not be encoded (%v); dropped", err)
		return ""
	}
	header := fmt.Sprintf(`{"event_id":%q,"sent_at":%q,"dsn":%q,"sdk":{"name":%q,"version":%q}}`,
		e.EventID, time.Now().UTC().Format(time.RFC3339Nano), c.dsn.raw, sdkName, Version)
	body := make([]byte, 0, len(header)+len(payload)+64)
	body = append(body, header...)
	body = append(body, "\n{\"type\":\"event\",\"content_type\":\"application/json\"}\n"...)
	body = append(body, payload...)
	body = append(body, '\n')
	if len(body) > maxQueueBytes {
		c.debugf("errors: the event is over 1 MB; dropped")
		return ""
	}
	c.tr.send(laneErrors, &outbound{
		url:  c.dsn.envelopeURL(),
		body: body,
		header: map[string][]string{
			"Content-Type":  {"application/x-sentry-envelope"},
			"X-Sentry-Auth": {fmt.Sprintf("Sentry sentry_version=7, sentry_client=%s/%s, sentry_key=%s", sdkName, Version, c.dsn.publicKey)},
		},
	})
	c.debugf("errors: queued event %s", e.EventID)
	return e.EventID
}

func (c *client) callBeforeSend(e *Event) (out *Event) {
	defer func() {
		if r := recover(); r != nil {
			c.debugf("BeforeSend panicked (%v); sending the event unchanged", r)
			out = e
		}
	}()
	return c.opts.BeforeSend(e)
}

// truncateEvent applies the spec's field limits, so Argus doesn't cut them.
func truncateEvent(e *Event) {
	e.Release = truncate(e.Release, 200)
	e.Environment = truncate(e.Environment, 64)
	e.ServerName = truncate(e.ServerName, 200)
	e.Transaction = truncate(e.Transaction, 300)
	if len(e.Tags) > 50 {
		n := 0
		for k := range e.Tags {
			if n++; n > 50 {
				delete(e.Tags, k)
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

const maxChain = 10

// exceptionChain is err and its causes, deepest cause first, err last (spec
// §4). The stack goes on err itself unless an error carries its own.
func exceptionChain(err error, stack *Stacktrace) []Exception {
	var chain []error
	queue := []error{err}
	seen := map[any]bool{}
	for len(queue) > 0 && len(chain) < maxChain {
		cur := queue[0]
		queue = queue[1:]
		if cur == nil {
			continue
		}
		if k, ok := identity(cur); ok {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		chain = append(chain, cur)
		switch u := cur.(type) {
		case interface{ Unwrap() []error }:
			queue = append(queue, u.Unwrap()...)
		case interface{ Unwrap() error }:
			queue = append(queue, u.Unwrap())
		}
	}
	out := make([]Exception, len(chain))
	for i, e := range chain {
		ex := Exception{Type: errorType(e), Value: e.Error()}
		if pcs := errorStack(e); len(pcs) > 0 {
			ex.Stacktrace = framesFromPCs(pcs)
		}
		out[len(chain)-1-i] = ex
	}
	if last := &out[len(out)-1]; last.Stacktrace == nil {
		last.Stacktrace = stack
	}
	return out
}

func errorType(err error) string {
	t := reflect.TypeOf(err)
	s := t.String()
	if t.Kind() == reflect.Pointer && t.Elem().PkgPath() != "" {
		return "*" + t.Elem().PkgPath() + "." + t.Elem().Name()
	}
	if t.PkgPath() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return s
}

// identity is a key for a value when comparing it can't panic: pointers and
// plain scalars. A struct holding a slice behind an interface would panic on ==.
func identity(v any) (any, bool) {
	if v == nil {
		return nil, false
	}
	switch reflect.TypeOf(v).Kind() {
	case reflect.Pointer, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Bool:
		return v, true
	}
	return nil, false
}

// panicMarks remembers recently captured panic values, so a panic seen by
// Middleware and again by an outer Recover is sent once.
var panicMarks struct {
	sync.Mutex
	ring [32]struct {
		v  any
		at time.Time
	}
	next int
}

// markPanic reports whether v is new (not captured in the last 5 seconds).
// Only pointers are remembered: two panics with the same string are usually
// two failures, and both should be sent.
func markPanic(v any) bool {
	if v == nil || reflect.TypeOf(v).Kind() != reflect.Pointer {
		return true
	}
	k := v
	panicMarks.Lock()
	defer panicMarks.Unlock()
	now := time.Now()
	for _, m := range panicMarks.ring {
		if m.v != nil && m.v == k && now.Sub(m.at) < 5*time.Second {
			return false
		}
	}
	panicMarks.ring[panicMarks.next] = struct {
		v  any
		at time.Time
	}{k, now}
	panicMarks.next = (panicMarks.next + 1) % len(panicMarks.ring)
	return true
}

func (c *client) capturePanic(ctx context.Context, v any, level Level) string {
	if !markPanic(v) {
		return ""
	}
	stack := callerStack()
	var ex []Exception
	if err, ok := v.(error); ok {
		ex = exceptionChain(err, stack)
	} else {
		ex = []Exception{{Type: "panic", Value: fmt.Sprint(v), Stacktrace: stack}}
	}
	return c.capture(ctx, &Event{Exception: ex}, Details{Level: level, Tags: map[string]string{"mechanism": "panic"}})
}
