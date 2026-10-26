package argus

import (
	"context"
	"time"
)

// Logger sends logs to Argus Logs. Use the package's Log value:
//
//	argus.Log.Info(ctx, "order placed", "orderId", "ord_81", "total", 18500)
//
// args are slog-style: alternating keys and values, or slog.Attr values.
// Nothing is printed locally; use NewSlogHandler to keep local output too.
type Logger struct{}

// Log is the package's Logger.
var Log Logger

// OTLP severity numbers (spec §5).
const (
	sevTrace = 1
	sevDebug = 5
	sevInfo  = 9
	sevWarn  = 13
	sevError = 17
	sevFatal = 21
)

// Trace logs at TRACE.
func (Logger) Trace(ctx context.Context, msg string, args ...any) { logAt(ctx, sevTrace, msg, args) }

// Debug logs at DEBUG.
func (Logger) Debug(ctx context.Context, msg string, args ...any) { logAt(ctx, sevDebug, msg, args) }

// Info logs at INFO.
func (Logger) Info(ctx context.Context, msg string, args ...any) { logAt(ctx, sevInfo, msg, args) }

// Warn logs at WARN.
func (Logger) Warn(ctx context.Context, msg string, args ...any) { logAt(ctx, sevWarn, msg, args) }

// Error logs at ERROR. It does not create an error event; use CaptureException for that.
func (Logger) Error(ctx context.Context, msg string, args ...any) { logAt(ctx, sevError, msg, args) }

// Fatal logs at FATAL. It does not exit.
func (Logger) Fatal(ctx context.Context, msg string, args ...any) { logAt(ctx, sevFatal, msg, args) }

func logAt(ctx context.Context, sev int, msg string, args []any) {
	c := get()
	if c == nil {
		return
	}
	defer c.guard()
	attrs, _, _ := collectAttrs(args)
	c.log(ctx, sev, msg, attrs)
}

func severityText(sev int) string {
	switch {
	case sev <= 4:
		return "TRACE"
	case sev <= 8:
		return "DEBUG"
	case sev <= 12:
		return "INFO"
	case sev <= 16:
		return "WARN"
	case sev <= 20:
		return "ERROR"
	default:
		return "FATAL"
	}
}

const (
	maxLogBody   = 16 << 10
	maxLogBuffer = 10 * maxLogBatch
)

func (c *client) log(ctx context.Context, sev int, msg string, attrs []attr) {
	sev = min(24, max(1, sev))
	r := logRecord{
		time:     time.Now(),
		severity: sev,
		text:     severityText(sev),
		body:     truncate(scrubString(msg), maxLogBody),
		attrs:    scrubAttrs(attrs),
	}
	if span := SpanFromContext(ctx); span != nil {
		r.traceID, r.spanID = span.traceID, span.spanID
	}
	c.logMu.Lock()
	c.logBuf = append(c.logBuf, r)
	if len(c.logBuf) > maxLogBuffer {
		c.logBuf = c.logBuf[len(c.logBuf)-maxLogBuffer:]
	}
	full := len(c.logBuf) >= maxLogBatch
	c.logMu.Unlock()
	if full {
		c.poke()
	}
}

func (c *client) flushLogs() {
	c.logMu.Lock()
	buf := c.logBuf
	c.logBuf = nil
	c.logMu.Unlock()
	for len(buf) > 0 {
		n := min(len(buf), maxLogBatch)
		batch := buf[:n]
		buf = buf[n:]
		splitEncode(len(batch), maxBodyBytes,
			func(lo, hi int) []byte { return logsRequest(c.resource, batch[lo:hi]) },
			func(b []byte) { c.sendOTLP(laneLogs, "/v1/logs", b) })
	}
}
