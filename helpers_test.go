package argus

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The key is a run of zeros: not a credential.
const testKey = "00000000000000000000000000000000"

type hit struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type reply struct {
	status int
	header map[string]string
	body   string
	delay  time.Duration
}

// stub is a stand-in Argus ingest on loopback. respond picks the answer for
// the nth (0-based) request on a path; nil answers 200.
type stub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	hits    []hit
	counts  map[string]int
	respond func(h hit, n int) reply
}

func newStub(t *testing.T, respond func(h hit, n int) reply) *stub {
	t.Helper()
	s := &stub{counts: map[string]int{}, respond: respond}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h := hit{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: b}
		s.mu.Lock()
		n := s.counts[r.URL.Path]
		s.counts[r.URL.Path]++
		s.hits = append(s.hits, h)
		s.mu.Unlock()
		rep := reply{status: 200}
		if s.respond != nil {
			rep = s.respond(h, n)
			if rep.status == 0 {
				rep.status = 200
			}
		}
		if rep.delay > 0 {
			time.Sleep(rep.delay)
		}
		for k, v := range rep.header {
			w.Header().Set(k, v)
		}
		if rep.body == "" && strings.HasPrefix(r.URL.Path, "/v1/flags") && rep.status == 200 {
			rep.body = `{"flags":{}}`
		}
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) dsn() string {
	return "http://" + testKey + "@" + strings.TrimPrefix(s.srv.URL, "http://") + "/42"
}

func (s *stub) byPath(prefix string) []hit {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []hit
	for _, h := range s.hits {
		if strings.HasPrefix(h.Path, prefix) {
			out = append(out, h)
		}
	}
	return out
}

// waitFor waits until at least n requests hit prefix.
func (s *stub) waitFor(t *testing.T, prefix string, n int) []hit {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.byPath(prefix)
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: got %d requests, want %d", prefix, len(got), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start points the SDK at the stub, with backoff in milliseconds.
func start(t *testing.T, s *stub, opts Options) {
	t.Helper()
	clearEnv(t)
	if s != nil {
		opts.DSN = s.dsn()
	}
	opts.timeUnit = time.Millisecond
	if err := Init(opts); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { Close(time.Second) })
}

type envelope struct {
	Header     map[string]any
	ItemHeader map[string]any
	Event      map[string]any
}

func parseEnvelope(t *testing.T, body []byte) envelope {
	t.Helper()
	lines := bytes.Split(bytes.TrimRight(body, "\n"), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("envelope has %d lines, want 3:\n%s", len(lines), body)
	}
	var e envelope
	for i, dst := range []*map[string]any{&e.Header, &e.ItemHeader, &e.Event} {
		if err := json.Unmarshal(lines[i], dst); err != nil {
			t.Fatalf("line %d: %v\n%s", i+1, err, lines[i])
		}
	}
	return e
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return m
}

// path walks decoded JSON: keys for objects, ints for arrays.
func path(t *testing.T, v any, steps ...any) any {
	t.Helper()
	cur := v
	for _, s := range steps {
		switch k := s.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("at %q: not an object: %v", k, cur)
			}
			cur = m[k]
		case int:
			a, ok := cur.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("at [%d]: not an array of that length: %v", k, cur)
			}
			cur = a[k]
		}
	}
	return cur
}

// otlpAttrs turns an OTLP attribute list into key → value object.
func otlpAttrs(t *testing.T, v any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, _ := v.([]any)
	for _, x := range list {
		m := x.(map[string]any)
		out[m["key"].(string)] = m["value"].(map[string]any)
	}
	return out
}

func clearEnv(t *testing.T) {
	for _, k := range []string{"ARGUS_DSN", "SENTRY_DSN", "ARGUS_RELEASE", "SENTRY_RELEASE", "ARGUS_ENVIRONMENT", "SENTRY_ENVIRONMENT", "ARGUS_SERVICE_NAME", "ARGUS_DEBUG"} {
		t.Setenv(k, "")
	}
}
