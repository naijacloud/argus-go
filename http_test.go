package argus

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const incomingTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func spansOf(t *testing.T, s *stub) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, h := range s.byPath("/v1/traces") {
		for _, sp := range path(t, decode(t, h.Body), "resourceSpans", 0, "scopeSpans", 0, "spans").([]any) {
			out = append(out, sp.(map[string]any))
		}
	}
	return out
}

func TestMiddlewareTracesRequests(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		SetTag(r.Context(), "order", r.PathValue("id"))
		Log.Info(r.Context(), "looked up order")
		CaptureMessage(r.Context(), "slow order lookup", LevelWarning)
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(Middleware(mux))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/orders/7?token=secret", nil)
	req.Header.Set("traceparent", incomingTP)
	req.Header.Set("Cookie", "session=abc")
	req.Header.Set("Authorization", "Bearer abcdefghijklmnop")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", res.StatusCode)
	}
	Flush(time.Second)

	spans := spansOf(t, s)
	if len(spans) != 1 {
		t.Fatalf("want one server span, got %d", len(spans))
	}
	sp := spans[0]
	wantName := "GET /orders/7"
	if patternSupported {
		wantName = "GET /orders/{id}"
	}
	if sp["name"] != wantName || sp["kind"] != 2.0 {
		t.Errorf("span %v %v", sp["name"], sp["kind"])
	}
	if sp["traceId"] != "4bf92f3577b34da6a3ce929d0e0e4736" || sp["parentSpanId"] != "00f067aa0ba902b7" {
		t.Error("the caller's traceparent must be continued")
	}
	a := otlpAttrs(t, sp["attributes"])
	if a["http.request.method"]["stringValue"] != "GET" || a["http.response.status_code"]["intValue"] != "201" {
		t.Errorf("attributes %v", a)
	}
	if u := a["url.full"]["stringValue"].(string); strings.Contains(u, "token") || !strings.HasSuffix(u, "/orders/7") {
		t.Errorf("url.full %s", u)
	}
	if patternSupported && a["http.route"]["stringValue"] != "/orders/{id}" {
		t.Errorf("http.route %v", a["http.route"])
	}

	ev := parseEnvelope(t, s.waitFor(t, "/api/", 1)[0].Body).Event
	if path(t, ev, "tags", "order") != "7" || ev["transaction"] != wantName {
		t.Errorf("event tags/transaction: %v %v", ev["tags"], ev["transaction"])
	}
	reqInfo := path(t, ev, "request").(map[string]any)
	if reqInfo["method"] != "GET" || strings.Contains(reqInfo["url"].(string), "?") {
		t.Errorf("request %v", reqInfo)
	}
	headers := reqInfo["headers"].(map[string]any)
	if _, has := headers["Cookie"]; has {
		t.Error("cookies are never sent")
	}
	if headers["Authorization"] != Filtered {
		t.Errorf("Authorization header: %v", headers["Authorization"])
	}
	logs := s.waitFor(t, "/v1/logs", 1)
	rec := path(t, decode(t, logs[0].Body), "resourceLogs", 0, "scopeLogs", 0, "logRecords", 0)
	if path(t, rec, "traceId") != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Error("logs in a request join its trace")
	}

	// Another request has its own scope.
	res, _ = http.Get(srv.URL + "/missing")
	res.Body.Close()
	Flush(time.Second)
	last := spansOf(t, s)[1]
	if otlpAttrs(t, last["attributes"])["http.response.status_code"]["intValue"] != "404" || last["traceId"] == "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("second request %v", last)
	}
}

func TestMiddlewareReportsPanics(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	boom := errors.New("nil map write")
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		AddBreadcrumb(r.Context(), Breadcrumb{Message: "about to fail"})
		panic(boom)
	}))
	func() {
		defer func() {
			if v := recover(); v != boom {
				t.Errorf("the panic must carry on to net/http, got %v", v)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/pay", nil))
	}()
	Flush(time.Second)
	ev := parseEnvelope(t, s.waitFor(t, "/api/", 1)[0].Body).Event
	if ev["level"] != "error" || path(t, ev, "tags", "mechanism") != "panic" || path(t, ev, "request", "method") != "POST" {
		t.Errorf("event %v", ev)
	}
	ex := path(t, ev, "exception", "values", 0)
	if path(t, ex, "value") != "nil map write" {
		t.Errorf("exception %v", ex)
	}
	frames := path(t, ex, "stacktrace", "frames").([]any)
	newest := frames[len(frames)-1].(map[string]any)
	if !strings.HasPrefix(newest["function"].(string), "TestMiddlewareReportsPanics") {
		t.Errorf("the newest frame is where it panicked: %v", newest["function"])
	}
	if path(t, ev, "breadcrumbs", "values", 0, "message") != "about to fail" {
		t.Error("request breadcrumbs")
	}
	sp := spansOf(t, s)[0]
	if path(t, sp, "status", "code") != 2.0 || otlpAttrs(t, sp["attributes"])["http.response.status_code"]["intValue"] != "500" {
		t.Errorf("span %v", sp)
	}
}

func TestPanicSentOnceThroughNestedRecovers(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	boom := errors.New("once")
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer RecoverWithContext(r.Context())
		panic(boom)
	}))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	Flush(time.Second)
	time.Sleep(20 * time.Millisecond)
	if n := len(s.byPath("/api/")); n != 1 {
		t.Fatalf("want one event, got %d", n)
	}
}

func TestMiddlewareIgnoresAbortHandler(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	h := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	Flush(time.Second)
	if n := len(s.byPath("/api/")); n != 0 {
		t.Fatal("http.ErrAbortHandler is not an error")
	}
}

func TestMiddlewareKeepsFlusher(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	var flushed bool
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, flushed = w.(http.Flusher)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController: %v", err)
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !flushed {
		t.Error("the writer must still be an http.Flusher")
	}
}

func TestRoundTripper(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	var gotTP string
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTP = r.Header.Get("traceparent")
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()
	client := &http.Client{Transport: RoundTripper(nil)}

	ctx, parent := StartSpan(context.Background(), "checkout")
	req, _ := http.NewRequestWithContext(ctx, "GET", down.URL+"/charge?secret=1", nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if req.Header.Get("traceparent") != "" {
		t.Error("the caller's request must not be modified")
	}
	parent.End()

	if !strings.HasPrefix(gotTP, "00-"+parent.TraceID()+"-") || strings.Contains(gotTP, parent.SpanID()) {
		t.Errorf("downstream traceparent %q should name the client span", gotTP)
	}
	CaptureMessage(ctx, "after call", "")
	Flush(time.Second)
	spans := spansOf(t, s)
	if len(spans) != 2 {
		t.Fatalf("spans %d", len(spans))
	}
	cs := spans[0]
	a := otlpAttrs(t, cs["attributes"])
	if cs["kind"] != 3.0 || cs["parentSpanId"] != parent.SpanID() || a["http.response.status_code"]["intValue"] != "502" ||
		strings.Contains(a["url.full"]["stringValue"].(string), "secret") || path(t, cs, "status", "code") != 2.0 {
		t.Errorf("client span %v", cs)
	}
	if !strings.HasSuffix(gotTP, cs["spanId"].(string)+"-01") {
		t.Error("traceparent carries the client span id")
	}
	ev := parseEnvelope(t, s.waitFor(t, "/api/", 1)[0].Body).Event
	crumb := path(t, ev, "breadcrumbs", "values", 0)
	if path(t, crumb, "category") != "http" || path(t, crumb, "data", "status_code") != 502.0 {
		t.Errorf("breadcrumb %v", crumb)
	}

	// Outside a span: no span, no header.
	gotTP = "unset"
	res, err = client.Get(down.URL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if gotTP != "" {
		t.Errorf("no traceparent outside a span, got %q", gotTP)
	}
}
