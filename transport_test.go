package argus

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRetriesServerErrors(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply {
		if n < 2 {
			return reply{status: 503}
		}
		return reply{status: 200}
	})
	start(t, s, Options{})
	CaptureMessage(context.Background(), "m", "")
	if !Flush(3 * time.Second) {
		t.Fatal("flush timed out")
	}
	hits := s.byPath("/api/")
	if len(hits) != 3 {
		t.Fatalf("want 2 failures then a success, got %d tries", len(hits))
	}
	if string(hits[0].Body) != string(hits[2].Body) {
		t.Error("a retry must resend the same envelope")
	}
}

func TestGivesUpAfterFiveTries(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply { return reply{status: 500} })
	start(t, s, Options{})
	CaptureMessage(context.Background(), "m", "")
	Flush(5 * time.Second)
	if n := len(s.byPath("/api/")); n != maxTries {
		t.Fatalf("want %d tries, got %d", maxTries, n)
	}
}

func TestBackoffSchedule(t *testing.T) {
	tr := newTransport(http.DefaultClient, time.Second, func(string, ...any) {})
	defer tr.close()
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		if got := tr.backoff(i); got != w*time.Second {
			t.Errorf("try %d: %s, want %s", i, got, w*time.Second)
		}
	}
}

func TestClientErrorsAreNotRetried(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply {
		if n == 0 {
			return reply{status: 400}
		}
		return reply{status: 413}
	})
	start(t, s, Options{})
	CaptureMessage(context.Background(), "a", "")
	Flush(time.Second)
	CaptureMessage(context.Background(), "b", "")
	Flush(time.Second)
	if n := len(s.byPath("/api/")); n != 2 {
		t.Fatalf("400/413: one try each and the endpoint stays open; got %d", n)
	}
}

func TestUnauthorizedStopsTheEndpoint(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := newStub(t, func(h hit, n int) reply {
				if strings.HasPrefix(h.Path, "/api/") {
					return reply{status: status}
				}
				return reply{}
			})
			start(t, s, Options{})
			CaptureMessage(context.Background(), "a", "")
			Flush(time.Second)
			CaptureMessage(context.Background(), "b", "")
			CaptureMessage(context.Background(), "c", "")
			Flush(time.Second)
			if n := len(s.byPath("/api/")); n != 1 {
				t.Fatalf("after %d nothing more goes to that endpoint; got %d requests", status, n)
			}
			// Other endpoints carry on.
			Log.Info(context.Background(), "still logging")
			Flush(time.Second)
			if n := len(s.byPath("/v1/logs")); n != 1 {
				t.Fatalf("logs should still be sent, got %d", n)
			}
		})
	}
}

func TestRateLimitWaitsRetryAfter(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply {
		if n == 0 {
			return reply{status: 429, header: map[string]string{"Retry-After": "40"}}
		}
		return reply{}
	})
	start(t, s, Options{}) // 40 "seconds" are 40 ms here
	began := time.Now()
	CaptureMessage(context.Background(), "a", "")
	hits := s.waitFor(t, "/api/", 2)
	if waited := time.Since(began); waited < 40*time.Millisecond {
		t.Errorf("retried after %s, before Retry-After", waited)
	}
	if string(hits[0].Body) != string(hits[1].Body) {
		t.Error("the limited request is sent again after the wait")
	}
}

func TestRateLimitBlocksTheLane(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply {
		if n == 0 {
			return reply{status: 429, header: map[string]string{"X-Sentry-Rate-Limits": "200:error;default:organization"}}
		}
		return reply{}
	})
	start(t, s, Options{})
	CaptureMessage(context.Background(), "a", "")
	s.waitFor(t, "/api/", 1)
	CaptureMessage(context.Background(), "b", "")
	time.Sleep(100 * time.Millisecond)
	if n := len(s.byPath("/api/")); n != 1 {
		t.Fatalf("nothing may be sent while rate limited, got %d", n)
	}
	s.waitFor(t, "/api/", 3) // a again, then b, once the 200 "seconds" pass
}

func TestRetryAfterParsing(t *testing.T) {
	cases := []struct {
		h    map[string]string
		want time.Duration
	}{
		{map[string]string{"Retry-After": "5"}, 5 * time.Second},
		{map[string]string{"Retry-After": "0.2"}, time.Second},
		{map[string]string{"Retry-After": "99999"}, time.Hour},
		{map[string]string{"X-Sentry-Rate-Limits": "30:error;default:key"}, 30 * time.Second},
		{map[string]string{}, 60 * time.Second},
		{map[string]string{"Retry-After": "soon"}, 60 * time.Second},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.h {
			h.Set(k, v)
		}
		if got := retryAfter(h); got != c.want {
			t.Errorf("%v: %s, want %s", c.h, got, c.want)
		}
	}
}

func TestNetworkErrorsNeverSurface(t *testing.T) {
	clearEnv(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // nothing listens there now
	if err := Init(Options{DSN: "http://" + testKey + "@" + addr + "/42", timeUnit: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	defer Close(time.Second)
	began := time.Now()
	CaptureException(context.Background(), errors.New("x"))
	Log.Info(context.Background(), "x")
	if time.Since(began) > 50*time.Millisecond {
		t.Fatal("capturing must not wait for the network")
	}
	if !Flush(5 * time.Second) {
		t.Fatal("retries should finish")
	}
}

func TestQueueIsBounded(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	tr := newTransport(srv.Client(), time.Millisecond, func(string, ...any) {})
	defer tr.close()
	for i := 0; i < 250; i++ {
		if !tr.send("x", &outbound{url: srv.URL, body: []byte("payload")}) {
			t.Fatal("send must accept while the lane is alive")
		}
	}
	l := tr.lane("x")
	l.mu.Lock()
	n := len(l.queue)
	l.mu.Unlock()
	if n > maxQueueItems {
		t.Fatalf("queue holds %d items, max %d", n, maxQueueItems)
	}
	big := make([]byte, 600<<10)
	tr.send("y", &outbound{url: srv.URL, body: big})
	tr.send("y", &outbound{url: srv.URL, body: big})
	tr.send("y", &outbound{url: srv.URL, body: big})
	ly := tr.lane("y")
	ly.mu.Lock()
	bytes := ly.bytes
	ly.mu.Unlock()
	if bytes > maxQueueBytes {
		t.Fatalf("queue holds %d bytes, max %d", bytes, maxQueueBytes)
	}
}

func TestFlushTimesOut(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply { return reply{delay: 300 * time.Millisecond} })
	start(t, s, Options{})
	CaptureMessage(context.Background(), "slow", "")
	if Flush(20 * time.Millisecond) {
		t.Fatal("Flush should report a timeout")
	}
	if !Flush(2 * time.Second) {
		t.Fatal("and then finish")
	}
}
