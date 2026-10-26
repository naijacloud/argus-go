package argus

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func checkResource(t *testing.T, res any) {
	t.Helper()
	a := otlpAttrs(t, path(t, res, "attributes"))
	want := map[string]string{
		"service.name":           "argus-go-test",
		"service.version":        "9.9.9",
		"deployment.environment": "test",
		"telemetry.sdk.name":     "argus.go",
		"telemetry.sdk.version":  Version,
	}
	for k, v := range want {
		if a[k]["stringValue"] != v {
			t.Errorf("resource %s = %v, want %s", k, a[k], v)
		}
	}
}

func otlpOptions() Options {
	return Options{ServiceName: "argus-go-test", Release: "9.9.9", Environment: "test"}
}

func TestLogsShape(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, otlpOptions())
	ctx, span := StartSpan(context.Background(), "job")
	Log.Info(ctx, "order placed", "orderId", "ord_81", "total", 18500, "token", "abc", slog.Float64("ratio", 0.5), slog.Bool("ok", true), slog.Group("req", "id", 7))
	Log.Error(context.Background(), "Authorization: Bearer abcdefghijklmnop failed")
	span.End()
	if !Flush(time.Second) {
		t.Fatal("flush")
	}
	h := s.waitFor(t, "/v1/logs", 1)[0]
	if h.Header.Get("Authorization") != "Bearer "+testKey || !strings.HasPrefix(h.Header.Get("Content-Type"), "application/json") {
		t.Errorf("headers %v", h.Header)
	}
	body := decode(t, h.Body)
	rl := path(t, body, "resourceLogs", 0)
	checkResource(t, path(t, rl, "resource"))
	recs := path(t, rl, "scopeLogs", 0, "logRecords").([]any)
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	r := recs[0]
	if path(t, r, "severityNumber") != 9.0 || path(t, r, "severityText") != "INFO" || path(t, r, "body", "stringValue") != "order placed" {
		t.Errorf("record %v", r)
	}
	if path(t, r, "traceId") != span.TraceID() || path(t, r, "spanId") != span.SpanID() {
		t.Error("a log inside a span carries its ids")
	}
	if _, ok := path(t, r, "timeUnixNano").(string); !ok {
		t.Error("timeUnixNano is a string")
	}
	a := otlpAttrs(t, path(t, r, "attributes"))
	if a["orderId"]["stringValue"] != "ord_81" || a["total"]["intValue"] != "18500" || a["ratio"]["doubleValue"] != 0.5 ||
		a["ok"]["boolValue"] != true || a["req.id"]["intValue"] != "7" || a["token"]["stringValue"] != Filtered {
		t.Errorf("attributes %v", a)
	}
	r2 := recs[1]
	if path(t, r2, "severityNumber") != 17.0 || path(t, r2, "body", "stringValue") != "Authorization: Bearer "+Filtered+" failed" {
		t.Errorf("error record %v", r2)
	}
	if _, has := r2.(map[string]any)["traceId"]; has {
		t.Error("no trace id outside a span")
	}
}

func TestLogBatchesAreSplit(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	line := strings.Repeat("x", 2000)
	for i := 0; i < 1000; i++ {
		Log.Info(context.Background(), line, "i", i)
	}
	if !Flush(5 * time.Second) {
		t.Fatal("flush")
	}
	total := 0
	hits := s.byPath("/v1/logs")
	for _, h := range hits {
		if len(h.Body) > maxBodyBytes {
			t.Errorf("body of %d bytes is over the limit", len(h.Body))
		}
		total += len(path(t, decode(t, h.Body), "resourceLogs", 0, "scopeLogs", 0, "logRecords").([]any))
	}
	if len(hits) < 2 || total != 1000 {
		t.Fatalf("want 1000 records over several requests, got %d over %d", total, len(hits))
	}
}

func TestMetricsShape(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, otlpOptions())
	ctx := context.Background()
	Metrics.Count(ctx, "orders.placed", 1, "method", "card")
	Metrics.Count(ctx, "orders.placed", 2, "method", "card")
	Metrics.Count(ctx, "orders.placed", 5, "method", "transfer")
	Metrics.Gauge(ctx, "queue.depth", 4)
	Metrics.Gauge(ctx, "queue.depth", 7)
	Metrics.Histogram(ctx, "checkout.duration", 3, Unit("ms"))
	Metrics.Histogram(ctx, "checkout.duration", 70, Unit("ms"))
	Metrics.Histogram(ctx, "checkout.duration", 20000, Unit("ms"))
	Flush(time.Second)
	h := s.waitFor(t, "/v1/metrics", 1)[0]
	rm := path(t, decode(t, h.Body), "resourceMetrics", 0)
	checkResource(t, path(t, rm, "resource"))
	got := map[string]map[string]any{}
	for _, m := range path(t, rm, "scopeMetrics", 0, "metrics").([]any) {
		mm := m.(map[string]any)
		key := mm["name"].(string)
		if sum, ok := mm["sum"].(map[string]any); ok {
			key += "|" + otlpAttrs(t, path(t, sum, "dataPoints", 0, "attributes"))["method"]["stringValue"].(string)
		}
		got[key] = mm
	}
	card := got["orders.placed|card"]
	if card == nil || path(t, card, "sum", "dataPoints", 0, "asDouble") != 3.0 ||
		path(t, card, "sum", "aggregationTemporality") != 1.0 || path(t, card, "sum", "isMonotonic") != true {
		t.Errorf("counter: %v", card)
	}
	if got["orders.placed|transfer"] == nil {
		t.Error("each attribute set is its own series")
	}
	if g := got["queue.depth"]; g == nil || path(t, g, "gauge", "dataPoints", 0, "asDouble") != 7.0 {
		t.Errorf("gauge: %v", g)
	}
	hist := got["checkout.duration"]
	if hist == nil || hist["unit"] != "ms" {
		t.Fatalf("histogram: %v", hist)
	}
	dp := path(t, hist, "histogram", "dataPoints", 0)
	if path(t, dp, "count") != "3" || path(t, dp, "sum") != 20073.0 || path(t, dp, "min") != 3.0 || path(t, dp, "max") != 20000.0 {
		t.Errorf("histogram point %v", dp)
	}
	buckets := path(t, dp, "bucketCounts").([]any)
	if len(buckets) != len(histogramBounds)+1 || buckets[0] != "1" || buckets[4] != "1" || buckets[len(buckets)-1] != "1" {
		t.Errorf("buckets %v", buckets)
	}
	if path(t, hist, "histogram", "aggregationTemporality") != 1.0 {
		t.Error("histograms are delta too")
	}

	// Delta: the next interval only has what happened since.
	Metrics.Count(ctx, "orders.placed", 1, "method", "card")
	Flush(time.Second)
	h2 := s.waitFor(t, "/v1/metrics", 2)[1]
	ms := path(t, decode(t, h2.Body), "resourceMetrics", 0, "scopeMetrics", 0, "metrics").([]any)
	if len(ms) != 1 || path(t, ms[0], "sum", "dataPoints", 0, "asDouble") != 1.0 {
		t.Errorf("second interval: %v", ms)
	}
	start1 := path(t, card, "sum", "dataPoints", 0, "timeUnixNano")
	start2 := path(t, ms[0], "sum", "dataPoints", 0, "startTimeUnixNano")
	if start1 != start2 {
		t.Error("each delta interval starts where the last ended")
	}
}

func TestDroppedSeriesHeaderIsNotRetried(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply {
		return reply{header: map[string]string{"X-Argus-Dropped-Series": "3"}}
	})
	var buf syncBuffer
	start(t, s, Options{Debug: true, DebugWriter: &buf})
	for i := 0; i < 2; i++ {
		Metrics.Count(context.Background(), "x", 1)
		Flush(time.Second)
	}
	if n := len(s.byPath("/v1/metrics")); n != 2 {
		t.Fatalf("want one request per interval, got %d", n)
	}
	if strings.Count(buf.String(), "series limit") != 1 {
		t.Errorf("log it once: %q", buf.String())
	}
}

func TestTracesShape(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, otlpOptions())
	ctx, parent := StartSpan(context.Background(), "checkout", "cart.items", 3)
	_, child := StartSpan(ctx, "charge card", SpanKindClient, "url.full", "https://user:pw@api.paystack.co/charge?token=abc#x", "password", "p")
	child.SetError(errors.New("declined"))
	child.End()
	parent.End()
	Flush(time.Second)
	h := s.waitFor(t, "/v1/traces", 1)[0]
	if h.Header.Get("Authorization") != "Bearer "+testKey {
		t.Error("auth header")
	}
	rs := path(t, decode(t, h.Body), "resourceSpans", 0)
	checkResource(t, path(t, rs, "resource"))
	spans := path(t, rs, "scopeSpans", 0, "spans").([]any)
	if len(spans) != 2 {
		t.Fatalf("spans %d", len(spans))
	}
	c, p := spans[0].(map[string]any), spans[1].(map[string]any)
	if p["name"] != "checkout" || p["kind"] != 1.0 || p["traceId"] != parent.TraceID() || p["spanId"] != parent.SpanID() {
		t.Errorf("parent %v", p)
	}
	if _, has := p["parentSpanId"]; has {
		t.Error("a root span has no parent")
	}
	if c["parentSpanId"] != parent.SpanID() || c["traceId"] != parent.TraceID() || c["kind"] != 3.0 {
		t.Errorf("child %v", c)
	}
	if path(t, c, "status", "code") != 2.0 || path(t, c, "status", "message") != "declined" || path(t, p, "status", "code") != 1.0 {
		t.Error("status")
	}
	a := otlpAttrs(t, c["attributes"])
	if a["url.full"]["stringValue"] != "https://api.paystack.co/charge" || a["password"]["stringValue"] != Filtered {
		t.Errorf("attributes %v", a)
	}
	if otlpAttrs(t, p["attributes"])["cart.items"]["intValue"] != "3" {
		t.Error("int attribute")
	}
	if c["startTimeUnixNano"].(string) > c["endTimeUnixNano"].(string) {
		t.Error("times")
	}
}

func TestTraceparent(t *testing.T) {
	sc, ok := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok || sc.traceID != "4bf92f3577b34da6a3ce929d0e0e4736" || sc.spanID != "00f067aa0ba902b7" || !sc.sampled {
		t.Fatalf("%+v %v", sc, ok)
	}
	if sc, ok := parseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"); !ok || sc.sampled {
		t.Error("unsampled flag")
	}
	for _, bad := range []string{"", "00-0000000000000000000000000000000-00f067aa0ba902b7-01", "00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "garbage"} {
		if _, ok := parseTraceparent(bad); ok {
			t.Errorf("%q should be refused", bad)
		}
	}
	s := newStub(t, nil)
	start(t, s, Options{})
	ctx := ContinueTrace(context.Background(), "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	_, span := StartSpan(ctx, "consume")
	if span.TraceID() != "4bf92f3577b34da6a3ce929d0e0e4736" || span.parentSpanID != "00f067aa0ba902b7" {
		t.Error("ContinueTrace")
	}
	if tp := span.Traceparent(); tp != "00-4bf92f3577b34da6a3ce929d0e0e4736-"+span.SpanID()+"-01" {
		t.Errorf("traceparent %s", tp)
	}
}

func TestTraceHelperAndErrors(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	err := Trace(context.Background(), "send invoices", func(ctx context.Context) error {
		if SpanFromContext(ctx) == nil {
			t.Error("fn runs inside the span")
		}
		CaptureException(ctx, errors.New("inside"))
		return errors.New("failed")
	})
	if err == nil || err.Error() != "failed" {
		t.Fatal("Trace returns fn's error")
	}
	func() {
		defer func() {
			if recover() != "boom" {
				t.Error("Trace must re-panic")
			}
		}()
		_ = Trace(context.Background(), "panics", func(context.Context) error { panic("boom") })
	}()
	Flush(time.Second)
	spans := path(t, decode(t, s.waitFor(t, "/v1/traces", 1)[0].Body), "resourceSpans", 0, "scopeSpans", 0, "spans").([]any)
	if len(spans) != 2 || path(t, spans[0], "status", "code") != 2.0 || path(t, spans[1], "status", "message") != "panic: boom" {
		t.Errorf("spans %v", spans)
	}
	ev := parseEnvelope(t, s.waitFor(t, "/api/", 1)[0].Body).Event
	if ev["transaction"] != "send invoices" || path(t, ev, "contexts", "trace", "trace_id") != path(t, spans[0], "traceId") {
		t.Errorf("an error inside a span names it: %v %v", ev["transaction"], ev["contexts"])
	}
}

func TestDisableTracing(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{DisableTracing: true})
	_, span := StartSpan(context.Background(), "x")
	span.End()
	Flush(time.Second)
	if span.Sampled() || len(s.byPath("/v1/traces")) != 0 {
		t.Fatal("no spans with DisableTracing")
	}
	if !strings.HasSuffix(span.Traceparent(), "-00") {
		t.Error("traceparent still forwarded, unsampled")
	}
}
