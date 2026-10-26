package argus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseDSN(t *testing.T) {
	d, err := parseDSN("https://abc123@argus-in.naijacloud.com/19")
	if err != nil {
		t.Fatal(err)
	}
	if d.publicKey != "abc123" || d.projectID != "19" || d.origin != "https://argus-in.naijacloud.com" {
		t.Fatalf("got %+v", d)
	}
	if got := d.envelopeURL(); got != "https://argus-in.naijacloud.com/api/19/envelope/" {
		t.Fatalf("envelope url %s", got)
	}
	d, err = parseDSN("https://k@host.example/prefix/path/7")
	if err != nil || d.origin != "https://host.example/prefix/path" || d.url("/v1/logs") != "https://host.example/prefix/path/v1/logs" {
		t.Fatalf("prefix: %+v %v", d, err)
	}
	for _, bad := range []string{"", "nonsense", "ftp://k@h/1", "https://h/1", "https://k@h/", "https://k@h/abc", "https://k@/1"} {
		if _, err := parseDSN(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestConfigOrder(t *testing.T) {
	clearEnv(t)
	s := newStub(t, nil)
	t.Setenv("SENTRY_DSN", "https://sentry@ignored.example/1")
	t.Setenv("ARGUS_DSN", s.dsn())
	t.Setenv("SENTRY_RELEASE", "sentry-rel")
	t.Setenv("ARGUS_ENVIRONMENT", "staging")
	t.Setenv("SENTRY_ENVIRONMENT", "ignored")
	if err := Init(Options{timeUnit: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	defer Close(time.Second)
	c := get()
	if c.dsn.raw != s.dsn() {
		t.Errorf("ARGUS_DSN should win over SENTRY_DSN, got %s", c.dsn.raw)
	}
	if c.opts.Release != "sentry-rel" || c.opts.Environment != "staging" {
		t.Errorf("release %q env %q", c.opts.Release, c.opts.Environment)
	}

	// The option wins over both.
	if err := Init(Options{DSN: "https://opt@argus.example/5", Release: "1.0", Environment: "dev"}); err != nil {
		t.Fatal(err)
	}
	c = get()
	if c.dsn.publicKey != "opt" || c.opts.Release != "1.0" || c.opts.Environment != "dev" {
		t.Errorf("options should win: %+v", c.opts)
	}
	Close(10 * time.Millisecond)

	clearEnv(t)
	t.Setenv("SENTRY_DSN", s.dsn())
	_ = Init(Options{})
	if c := get(); c == nil || c.opts.Environment != "production" {
		t.Fatalf("SENTRY_DSN fallback / production default not applied")
	}
	Close(time.Second)
}

func TestNoDSNIsANoOp(t *testing.T) {
	clearEnv(t)
	if err := Init(Options{}); err != nil {
		t.Fatalf("no DSN must not be an error: %v", err)
	}
	if Enabled() {
		t.Fatal("enabled without a DSN")
	}
	ctx := context.Background()
	if id := CaptureException(ctx, errors.New("x")); id != "" {
		t.Fatal("sent without a DSN")
	}
	CaptureMessage(ctx, "m", LevelInfo)
	CapturePanic(ctx, "p")
	SetTag(ctx, "a", "b")
	SetUser(ctx, User{ID: "1"})
	AddBreadcrumb(ctx, Breadcrumb{Message: "m"})
	Log.Info(ctx, "x", "k", "v")
	Metrics.Count(ctx, "c", 1)
	Metrics.Gauge(ctx, "g", 1)
	Metrics.Histogram(ctx, "h", 1)
	sctx, span := StartSpan(ctx, "s")
	if span != nil || sctx != ctx {
		t.Fatal("StartSpan before Init should return nil and the same ctx")
	}
	span.SetAttribute("k", 1)
	span.SetError(errors.New("x"))
	span.End()
	ran := false
	if err := Cron.CheckIn(ctx, "slug", func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatal("CheckIn must run the job without a DSN")
	}
	if Flags.IsOn(ctx, "f", "u") || !Flags.IsOn(ctx, "f", "u", FlagDefault(true)) || Flags.Variant(ctx, "f", "u", "fb") != "fb" {
		t.Fatal("flags must return the default without a DSN")
	}
	if !Flush(0) || !Close(0) {
		t.Fatal("Flush/Close without a DSN should be true")
	}
	if err := Trace(ctx, "t", func(context.Context) error { return errors.New("e") }); err == nil || err.Error() != "e" {
		t.Fatal("Trace must return fn's error")
	}
}

func TestInvalidDSN(t *testing.T) {
	clearEnv(t)
	if err := Init(Options{DSN: "not a dsn"}); err == nil {
		t.Fatal("want an error")
	}
	if Enabled() {
		t.Fatal("an invalid DSN must leave the SDK off")
	}
	CaptureException(context.Background(), errors.New("x"))
}

func TestDisabled(t *testing.T) {
	clearEnv(t)
	s := newStub(t, nil)
	_ = Init(Options{DSN: s.dsn(), Disabled: true})
	CaptureException(context.Background(), errors.New("x"))
	if Enabled() || len(s.byPath("/")) != 0 {
		t.Fatal("Disabled must send nothing")
	}
}

type codeError struct{ code int }

func (e *codeError) Error() string { return fmt.Sprintf("code %d", e.code) }

func TestEnvelopeShape(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{Release: "shop@1.4.2", ServerName: "web-1"})
	base := &codeError{402}
	err := fmt.Errorf("charge failed: %w", base)
	id := CaptureException(context.Background(), err, Details{
		Tags:  map[string]string{"provider": "paystack"},
		Extra: map[string]any{"password": "hunter2", "note": "card 4242 4242 4242 4242 declined", "ch": make(chan int)},
	})
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(id) {
		t.Fatalf("event id %q", id)
	}
	h := s.waitFor(t, "/api/42/envelope/", 1)[0]
	if ct := h.Header.Get("Content-Type"); ct != "application/x-sentry-envelope" {
		t.Errorf("content type %q", ct)
	}
	auth := h.Header.Get("X-Sentry-Auth")
	for _, want := range []string{"Sentry ", "sentry_version=7", "sentry_key=" + testKey, "sentry_client=argus.go/" + Version} {
		if !strings.Contains(auth, want) {
			t.Errorf("X-Sentry-Auth %q lacks %q", auth, want)
		}
	}
	env := parseEnvelope(t, h.Body)
	if env.Header["event_id"] != id || env.Header["dsn"] != s.dsn() {
		t.Errorf("envelope header %v", env.Header)
	}
	if _, err := time.Parse(time.RFC3339Nano, env.Header["sent_at"].(string)); err != nil {
		t.Errorf("sent_at: %v", err)
	}
	if path(t, env.Header, "sdk", "name") != "argus.go" || path(t, env.Header, "sdk", "version") != Version {
		t.Errorf("sdk %v", env.Header["sdk"])
	}
	if env.ItemHeader["type"] != "event" || env.ItemHeader["content_type"] != "application/json" {
		t.Errorf("item header %v", env.ItemHeader)
	}
	ev := env.Event
	for k, want := range map[string]any{
		"event_id": id, "platform": "go", "level": "error", "release": "shop@1.4.2",
		"environment": "production", "server_name": "web-1",
	} {
		if ev[k] != want {
			t.Errorf("%s = %v, want %v", k, ev[k], want)
		}
	}
	if path(t, ev, "sdk", "name") != "argus.go" {
		t.Error("event sdk.name")
	}
	if _, err := time.Parse(time.RFC3339Nano, ev["timestamp"].(string)); err != nil {
		t.Errorf("timestamp: %v", err)
	}
	vals := path(t, ev, "exception", "values").([]any)
	if len(vals) != 2 {
		t.Fatalf("want the error and its cause, got %d", len(vals))
	}
	if path(t, vals[0], "type") != "*github.com/naijacloud/argus-go.codeError" || path(t, vals[0], "value") != "code 402" {
		t.Errorf("cause first: %v", vals[0])
	}
	if path(t, vals[1], "type") != "*fmt.wrapError" || path(t, vals[1], "value") != "charge failed: code 402" {
		t.Errorf("error last: %v", vals[1])
	}
	frames := path(t, vals[1], "stacktrace", "frames").([]any)
	newest := frames[len(frames)-1].(map[string]any)
	if newest["function"] != "TestEnvelopeShape" || newest["in_app"] != true || newest["module"] != sdkPackage {
		t.Errorf("newest frame should be the capture site: %v", newest)
	}
	if !strings.HasSuffix(newest["abs_path"].(string), "client_test.go") || newest["lineno"].(float64) <= 0 {
		t.Errorf("frame location: %v", newest)
	}
	if cl, _ := newest["context_line"].(string); !strings.Contains(cl, "CaptureException") {
		t.Errorf("context_line %q", cl)
	}
	for _, f := range frames {
		if fn := f.(map[string]any)["module"]; fn == "runtime" {
			t.Error("runtime frames should be dropped")
		}
	}
	if path(t, ev, "extra", "password") != Filtered {
		t.Error("extra.password not filtered")
	}
	if note := path(t, ev, "extra", "note"); note != "card "+Filtered+" declined" {
		t.Errorf("card number not filtered: %v", note)
	}
	if ch := path(t, ev, "extra", "ch"); ch != "[chan int: not JSON]" {
		t.Errorf("unmarshallable value: %v", ch)
	}
	if path(t, ev, "tags", "provider") != "paystack" {
		t.Error("tag")
	}
	if path(t, ev, "contexts", "runtime", "name") != "go" {
		t.Error("runtime context")
	}
}

func TestCaptureMessage(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	CaptureMessage(context.Background(), "webhook signature mismatch token=abcdef123", LevelWarning)
	ev := parseEnvelope(t, s.waitFor(t, "/api/", 1)[0].Body).Event
	if ev["message"] != "webhook signature mismatch token="+Filtered || ev["level"] != "warning" {
		t.Errorf("got %v / %v", ev["message"], ev["level"])
	}
	if _, has := ev["exception"]; has {
		t.Error("a message has no exception")
	}
}

func TestScopes(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	bg := context.Background()
	SetTag(bg, "region", "af-west")
	a := WithScope(bg)
	b := WithScope(bg)
	SetTag(a, "req", "a")
	SetUser(a, User{ID: "u1", Email: "a@example.com", IPAddress: "10.0.0.1"})
	AddBreadcrumb(a, Breadcrumb{Category: "auth", Message: "signed in", Data: map[string]any{"token": "t0ps3cret", "url": "https://shop.ng/cb?code=123"}})
	SetContext(a, "order", map[string]any{"id": "ord_1", "cvv": "123"})
	SetTag(b, "req", "b")

	CaptureMessage(a, "from a", LevelInfo)
	CaptureMessage(b, "from b", LevelInfo)
	hits := s.waitFor(t, "/api/", 2)
	byMsg := map[string]map[string]any{}
	for _, h := range hits {
		ev := parseEnvelope(t, h.Body).Event
		byMsg[ev["message"].(string)] = ev
	}
	ea, eb := byMsg["from a"], byMsg["from b"]
	if path(t, ea, "tags", "req") != "a" || path(t, eb, "tags", "req") != "b" {
		t.Error("scopes leaked into each other")
	}
	if path(t, ea, "tags", "region") != "af-west" || path(t, eb, "tags", "region") != "af-west" {
		t.Error("global tag missing")
	}
	if path(t, ea, "user", "id") != "u1" {
		t.Error("user")
	}
	if _, has := path(t, ea, "user").(map[string]any)["ip_address"]; has {
		t.Error("ip_address must not be sent without SendDefaultPii")
	}
	if _, has := eb["user"]; has {
		t.Error("b has no user")
	}
	crumb := path(t, ea, "breadcrumbs", "values", 0)
	if path(t, crumb, "message") != "signed in" || path(t, crumb, "data", "token") != Filtered || path(t, crumb, "data", "url") != "https://shop.ng/cb" {
		t.Errorf("breadcrumb %v", crumb)
	}
	if _, has := eb["breadcrumbs"]; has {
		t.Error("b's breadcrumbs should be empty")
	}
	if path(t, ea, "contexts", "order", "cvv") != Filtered || path(t, ea, "contexts", "order", "id") != "ord_1" {
		t.Error("contexts scrubbing")
	}
}

func TestBreadcrumbLimit(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{MaxBreadcrumbs: 1000})
	ctx := WithScope(context.Background())
	for i := 0; i < 150; i++ {
		AddBreadcrumb(ctx, Breadcrumb{Message: fmt.Sprint(i)})
	}
	CaptureMessage(ctx, "m", "")
	ev := parseEnvelope(t, s.waitFor(t, "/api/", 1)[0].Body).Event
	vals := path(t, ev, "breadcrumbs", "values").([]any)
	if len(vals) != 100 || path(t, vals[0], "message") != "50" || path(t, vals[99], "message") != "149" {
		t.Fatalf("want the last 100, got %d starting %v", len(vals), path(t, vals[0], "message"))
	}
}

func TestBeforeSendAndPii(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{
		SendDefaultPii: true,
		BeforeSend: func(e *Event) *Event {
			switch e.Message {
			case "drop":
				return nil
			case "boom":
				panic("bad hook")
			}
			e.Tags = map[string]string{"edited": "yes"}
			return e
		},
		BeforeBreadcrumb: func(b *Breadcrumb) *Breadcrumb {
			if b.Category == "noise" {
				return nil
			}
			return b
		},
	})
	ctx := WithScope(context.Background())
	SetUser(ctx, User{ID: "1", IPAddress: "10.1.2.3"})
	AddBreadcrumb(ctx, Breadcrumb{Category: "noise"})
	if id := CaptureMessage(ctx, "drop", ""); id != "" {
		t.Error("BeforeSend nil must drop")
	}
	CaptureMessage(ctx, "keep", "")
	CaptureMessage(ctx, "boom", "")
	hits := s.waitFor(t, "/api/", 2)
	time.Sleep(20 * time.Millisecond)
	if n := len(s.byPath("/api/")); n != 2 {
		t.Fatalf("want 2 events, got %d", n)
	}
	for _, h := range hits {
		ev := parseEnvelope(t, h.Body).Event
		if ev["message"] == "keep" && path(t, ev, "tags", "edited") != "yes" {
			t.Error("BeforeSend edit lost")
		}
		if path(t, ev, "user", "ip_address") != "10.1.2.3" {
			t.Error("SendDefaultPii keeps the IP")
		}
		if _, has := ev["breadcrumbs"]; has {
			t.Error("BeforeBreadcrumb nil must drop the crumb")
		}
	}
}

func TestSampleRate(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{SampleRate: 0.000001})
	for i := 0; i < 20; i++ {
		CaptureMessage(context.Background(), "m", "")
	}
	Flush(time.Second)
	if n := len(s.byPath("/api/")); n > 1 {
		t.Fatalf("sampled out events were sent: %d", n)
	}
}

func TestCloseStops(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	CaptureMessage(context.Background(), "before", "")
	if !Close(2 * time.Second) {
		t.Fatal("Close should flush in time")
	}
	if n := len(s.byPath("/api/")); n != 1 {
		t.Fatalf("Close must send what was queued, got %d", n)
	}
	if id := CaptureMessage(context.Background(), "after", ""); id != "" || Enabled() {
		t.Fatal("nothing is sent after Close")
	}
}

func TestNeverPanics(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	var nilCtx context.Context // callers do pass nil; it must be tolerated
	CaptureException(nilCtx, errors.New("x"))
	CaptureException(nilCtx, nil)
	CaptureMessage(nilCtx, "x", "")
	SetTag(nilCtx, "a", "b")
	SetUser(nilCtx, User{})
	SetContext(nilCtx, "c", nil)
	SetExtra(nilCtx, "f", func() {})
	AddBreadcrumb(nilCtx, Breadcrumb{Data: map[string]any{"f": func() {}}})
	Log.Info(nilCtx, "x", 1, 2, 3)
	Metrics.Count(nilCtx, "", 1)
	Metrics.Gauge(nilCtx, "nan", nanValue())
	_, sp := StartSpan(nilCtx, "x", struct{ A []int }{})
	sp.SetAttribute("weird", []any{map[string]any{"x": 1}})
	sp.End()
	sp.End()
	CapturePanic(nilCtx, struct{ s []int }{[]int{1}})
	Flags.IsOn(nilCtx, "", "")
	_ = Cron.CheckIn(nilCtx, "", nil)
	ContinueTrace(nilCtx, "garbage")
	CaptureException(context.Background(), errors.Join(errors.New("a"), nil, fmt.Errorf("b: %w", errors.New("c"))))
	if !Flush(2 * time.Second) {
		t.Fatal("flush timed out")
	}
}

func nanValue() float64 { var z float64; return z / z }

func TestConcurrentUse(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx := WithScope(context.Background())
			for i := 0; i < 50; i++ {
				ctx2, span := StartSpan(ctx, "work", "g", g)
				SetTag(ctx2, "g", fmt.Sprint(g))
				AddBreadcrumb(ctx2, Breadcrumb{Message: "step"})
				Log.Info(ctx2, "tick", "i", i)
				Metrics.Count(ctx2, "ticks", 1, "g", g%2)
				Metrics.Histogram(ctx2, "ms", float64(i))
				if i%10 == 0 {
					CaptureException(ctx2, errors.New("periodic"))
				}
				span.End()
			}
		}(g)
	}
	wg.Wait()
	if !Flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if n := len(s.byPath("/api/")); n != 80 {
		t.Errorf("events: %d, want 80", n)
	}
}

func TestDebugOutput(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply { return reply{status: 400} })
	var buf syncBuffer
	start(t, s, Options{Debug: true, DebugWriter: &buf})
	CaptureMessage(context.Background(), "m", "")
	Flush(time.Second)
	if !strings.Contains(buf.String(), "[argus] errors: 400, dropped") {
		t.Errorf("debug output: %q", buf.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The SDK's own client must not be http.DefaultClient's transport.
func TestOwnHTTPTransport(t *testing.T) {
	if ownTransport() == http.DefaultTransport {
		t.Fatal("the SDK must not share http.DefaultTransport")
	}
}
