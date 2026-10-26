package argus

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// SpanKind is a span's OTLP kind. Pass one among StartSpan's arguments to
// set it; the default is SpanKindInternal.
type SpanKind int

// The OTLP span kinds Argus reads.
const (
	SpanKindInternal SpanKind = 1
	SpanKindServer   SpanKind = 2
	SpanKindClient   SpanKind = 3
	SpanKindProducer SpanKind = 4
	SpanKindConsumer SpanKind = 5
)

// Span is a unit of work in a trace. A nil *Span is valid and does nothing,
// so code using spans needs no checks when the SDK is off.
type Span struct {
	c            *client
	traceID      string
	spanID       string
	parentSpanID string
	sampled      bool
	kind         SpanKind
	start        time.Time

	// req is the incoming request of a Middleware span, read for its route.
	req *http.Request

	mu     sync.Mutex
	name   string
	attrs  []attr
	errMsg string
	ended  bool
}

type spanKey struct{}
type remoteParentKey struct{}

type spanContext struct {
	traceID, spanID string
	sampled         bool
}

// SpanFromContext returns the active span, or nil.
func SpanFromContext(ctx context.Context) *Span {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(spanKey{}).(*Span)
	return s
}

// ContextWithSpan returns ctx with span active.
func ContextWithSpan(ctx context.Context, span *Span) context.Context {
	return context.WithValue(orBackground(ctx), spanKey{}, span)
}

var traceparentRe = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-([0-9a-f]{2})$`)

// ContinueTrace makes the next span started from the returned context a
// child of a W3C traceparent from another service (a queue message, say).
// An invalid traceparent leaves ctx as it is. Middleware does this for
// incoming requests.
func ContinueTrace(ctx context.Context, traceparent string) context.Context {
	ctx = orBackground(ctx)
	sc, ok := parseTraceparent(traceparent)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, remoteParentKey{}, sc)
}

func parseTraceparent(h string) (spanContext, bool) {
	m := traceparentRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(h)))
	if m == nil || m[1] == "ff" || strings.Trim(m[2], "0") == "" || strings.Trim(m[3], "0") == "" {
		return spanContext{}, false
	}
	var flags int
	_, _ = fmt.Sscanf(m[4], "%x", &flags)
	return spanContext{traceID: m[2], spanID: m[3], sampled: flags&1 == 1}, true
}

// StartSpan starts a span under the context's active span (or a new trace)
// and returns a context carrying it. End it yourself. args are slog-style
// attributes, plus an optional SpanKind:
//
//	ctx, span := argus.StartSpan(ctx, "charge card", "provider", "paystack")
//	defer span.End()
//
// Before Init the span is nil and ctx is returned unchanged.
func StartSpan(ctx context.Context, name string, args ...any) (context.Context, *Span) {
	ctx = orBackground(ctx)
	c := get()
	if c == nil {
		return ctx, nil
	}
	attrs, _, kind := collectAttrs(args)
	return c.startSpan(ctx, name, kind, attrs)
}

func (c *client) startSpan(ctx context.Context, name string, kind SpanKind, attrs []attr) (context.Context, *Span) {
	if kind == 0 {
		kind = SpanKindInternal
	}
	s := &Span{c: c, name: name, kind: kind, spanID: newSpanID(), start: time.Now(), attrs: attrs}
	if parent := SpanFromContext(ctx); parent != nil {
		s.traceID, s.parentSpanID, s.sampled = parent.traceID, parent.spanID, parent.sampled
	} else if rp, ok := ctx.Value(remoteParentKey{}).(spanContext); ok {
		s.traceID, s.parentSpanID, s.sampled = rp.traceID, rp.spanID, rp.sampled && !c.opts.DisableTracing
	} else {
		s.traceID = newTraceID()
		s.sampled = !c.opts.DisableTracing && c.sample(c.opts.TracesSampleRate)
	}
	return context.WithValue(ctx, spanKey{}, s), s
}

// Trace runs fn inside a span named name. The span ends when fn returns, and
// is marked failed when fn returns an error or panics (the panic carries on).
//
//	err := argus.Trace(ctx, "send invoices", func(ctx context.Context) error {
//		return sendInvoices(ctx)
//	})
func Trace(ctx context.Context, name string, fn func(ctx context.Context) error, args ...any) (err error) {
	ctx, span := StartSpan(ctx, name, args...)
	defer func() {
		if v := recover(); v != nil {
			span.SetError(fmt.Errorf("panic: %v", v))
			span.End()
			panic(v)
		}
		span.SetError(err)
		span.End()
	}()
	return fn(ctx)
}

// Name returns the span's name.
func (s *Span) Name() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.name
}

// transaction is the span's name for events: for a request span, the
// matched route once the router has picked one.
func (s *Span) transaction() string {
	if s == nil {
		return ""
	}
	if s.req != nil {
		if v, ok := s.attribute("http.route"); ok {
			if r, _ := v.(string); r != "" {
				return s.req.Method + " " + r
			}
		}
		if r := routePattern(s.req); r != "" {
			return s.req.Method + " " + r
		}
	}
	return s.Name()
}

// SetName renames the span, for example once the route is known.
func (s *Span) SetName(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
}

// SetAttribute sets one attribute; a later value for the same key wins.
func (s *Span) SetAttribute(key string, value any) {
	if s == nil {
		return
	}
	var v any
	switch t := value.(type) {
	case int:
		v = int64(t)
	case int64, float64, bool, string:
		v = t
	default:
		v = anyValue(value)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.attrs {
		if s.attrs[i].key == key {
			s.attrs[i].val = v
			return
		}
	}
	s.attrs = append(s.attrs, attr{key, v})
}

func (s *Span) attribute(key string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.attrs {
		if a.key == key {
			return a.val, true
		}
	}
	return nil, false
}

// SetError marks the span failed with err's message. A nil err does nothing.
func (s *Span) SetError(err error) {
	if s == nil || err == nil {
		return
	}
	s.mu.Lock()
	s.errMsg = scrubString(err.Error())
	s.mu.Unlock()
}

func (s *Span) setErrorMessage(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.errMsg == "" {
		s.errMsg = msg
	}
	s.mu.Unlock()
}

// TraceID is the span's 32-hex trace id.
func (s *Span) TraceID() string {
	if s == nil {
		return ""
	}
	return s.traceID
}

// SpanID is the span's 16-hex id.
func (s *Span) SpanID() string {
	if s == nil {
		return ""
	}
	return s.spanID
}

// Sampled reports whether the span will be sent.
func (s *Span) Sampled() bool { return s != nil && s.sampled }

// Traceparent is the W3C traceparent header value for calls made inside the span.
func (s *Span) Traceparent() string {
	if s == nil {
		return ""
	}
	flags := "00"
	if s.sampled {
		flags = "01"
	}
	return "00-" + s.traceID + "-" + s.spanID + "-" + flags
}

// End ends the span and queues it. Later calls do nothing.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	d := spanData{
		traceID: s.traceID, spanID: s.spanID, parentSpanID: s.parentSpanID,
		name: s.name, kind: s.kind, start: s.start, end: time.Now(), errMsg: s.errMsg,
		attrs: append([]attr(nil), s.attrs...),
	}
	s.mu.Unlock()
	if !s.sampled || s.c == nil || s.c.closed.Load() {
		return
	}
	defer s.c.guard()
	s.c.recordSpan(d)
}

const maxSpanBuffer = 5000

func (c *client) recordSpan(d spanData) {
	d.attrs = scrubAttrs(d.attrs)
	c.spanMu.Lock()
	c.spanBuf = append(c.spanBuf, d)
	if len(c.spanBuf) > maxSpanBuffer {
		c.spanBuf = c.spanBuf[len(c.spanBuf)-maxSpanBuffer:]
	}
	full := len(c.spanBuf) >= maxSpanBatch
	c.spanMu.Unlock()
	if full {
		c.poke()
	}
}

func (c *client) flushSpans() {
	c.spanMu.Lock()
	buf := c.spanBuf
	c.spanBuf = nil
	c.spanMu.Unlock()
	for len(buf) > 0 {
		n := min(len(buf), maxSpanBatch)
		batch := buf[:n]
		buf = buf[n:]
		splitEncode(len(batch), maxBodyBytes,
			func(lo, hi int) []byte { return tracesRequest(c.resource, batch[lo:hi]) },
			func(b []byte) { c.sendOTLP(laneTraces, "/v1/traces", b) })
	}
}

func (c *client) sendOTLP(lane, path string, body []byte) {
	c.tr.send(lane, &outbound{
		url:  c.dsn.url(path),
		body: body,
		header: map[string][]string{
			"Content-Type":  {"application/json"},
			"Authorization": {"Bearer " + c.dsn.publicKey},
		},
	})
}
