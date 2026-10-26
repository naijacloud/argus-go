package argus

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sending (spec §6): one queue per endpoint, never blocking the app and never
// panicking. Network errors and 5xx back off and retry; 429 waits Retry-After;
// 401/403 stop the endpoint until restart; any other 4xx drops the request.

const (
	laneErrors  = "errors"
	laneLogs    = "logs"
	laneMetrics = "metrics"
	laneTraces  = "traces"
	laneCrons   = "crons"
	laneFlags   = "flags"

	maxQueueItems = 100
	maxQueueBytes = 1 << 20
	maxTries      = 5
	maxBackoff    = 60 // in time units (seconds)
	// A batch held back by 429s is retried this many times before it is dropped,
	// so a server that answers 429 forever can't pin a request in memory forever.
	max429Waits = 10
	// Bodies are split below this so one request fits the 1 MB queue and the
	// 1 MB ingest limit.
	maxBodyBytes = 900 << 10
)

type outbound struct {
	url    string
	body   []byte
	header http.Header
}

type lane struct {
	name string

	mu           sync.Mutex
	queue        []*outbound
	bytes        int
	running      bool
	blockedUntil time.Time
	dead         bool
}

type transport struct {
	httpc     *http.Client
	unit      time.Duration
	debugf    func(string, ...any)
	onSuccess func(lane string, res *http.Response)

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.Mutex
	lanes map[string]*lane
}

func newTransport(httpc *http.Client, unit time.Duration, debugf func(string, ...any)) *transport {
	ctx, cancel := context.WithCancel(internalContext(context.Background()))
	return &transport{httpc: httpc, unit: unit, debugf: debugf, ctx: ctx, cancel: cancel, lanes: map[string]*lane{}}
}

func (t *transport) lane(name string) *lane {
	t.mu.Lock()
	defer t.mu.Unlock()
	l := t.lanes[name]
	if l == nil {
		l = &lane{name: name}
		t.lanes[name] = l
	}
	return l
}

// available reports whether a lane may send now (not stopped, not rate limited).
func (t *transport) available(name string) bool {
	l := t.lane(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.dead && !time.Now().Before(l.blockedUntil)
}

// send queues a request. It never blocks; false means it was dropped.
func (t *transport) send(name string, o *outbound) bool {
	if t.ctx.Err() != nil {
		return false
	}
	l := t.lane(name)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.dead {
		return false
	}
	l.queue = append(l.queue, o)
	l.bytes += len(o.body)
	for len(l.queue) > 0 && (len(l.queue) > maxQueueItems || l.bytes > maxQueueBytes) {
		l.bytes -= len(l.queue[0].body)
		l.queue[0] = nil
		l.queue = l.queue[1:]
		t.debugf("%s: queue full, dropped the oldest request", name)
	}
	if !l.running && len(l.queue) > 0 {
		l.running = true
		t.wg.Add(1)
		go t.run(l)
	}
	return true
}

func (t *transport) run(l *lane) {
	defer t.wg.Done()
	for {
		l.mu.Lock()
		if len(l.queue) == 0 || l.dead || t.ctx.Err() != nil {
			l.running = false
			if l.dead || t.ctx.Err() != nil {
				l.queue, l.bytes = nil, 0
			}
			l.mu.Unlock()
			return
		}
		o := l.queue[0]
		l.queue[0] = nil
		l.queue = l.queue[1:]
		l.bytes -= len(o.body)
		l.mu.Unlock()
		t.deliver(l, o)
	}
}

func (t *transport) sleep(d time.Duration) bool {
	if d <= 0 {
		return t.ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-t.ctx.Done():
		return false
	}
}

func (t *transport) backoff(try int) time.Duration {
	s := 1 << try
	if s > maxBackoff {
		s = maxBackoff
	}
	return time.Duration(s) * t.unit
}

// deliver sends one request with the spec's retry rules.
func (t *transport) deliver(l *lane, o *outbound) {
	waits := 0
	for try := 0; try < maxTries; {
		l.mu.Lock()
		wait := time.Until(l.blockedUntil)
		l.mu.Unlock()
		if wait > 0 && !t.sleep(wait) {
			return
		}
		res, err := t.do(o)
		if err != nil {
			if t.ctx.Err() != nil {
				return
			}
			try++
			t.debugf("%s: network error (%v), try %d of %d", l.name, err, try, maxTries)
			if try < maxTries && !t.sleep(t.backoff(try-1)) {
				return
			}
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		_ = res.Body.Close()
		switch {
		case res.StatusCode >= 200 && res.StatusCode < 300:
			if t.onSuccess != nil {
				t.onSuccess(l.name, res)
			}
			return
		case res.StatusCode == http.StatusTooManyRequests:
			d := t.scaled(retryAfter(res.Header))
			l.mu.Lock()
			l.blockedUntil = time.Now().Add(d)
			l.mu.Unlock()
			waits++
			t.debugf("%s: 429, sending nothing on this endpoint for %s", l.name, d)
			if waits > max429Waits {
				return
			}
			continue // no extra try counted
		case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
			t.kill(l, res.StatusCode)
			return
		case res.StatusCode >= 400 && res.StatusCode < 500:
			t.debugf("%s: %d, dropped (not retried)", l.name, res.StatusCode)
			return
		default:
			try++
			t.debugf("%s: %d, try %d of %d", l.name, res.StatusCode, try, maxTries)
			if try < maxTries && !t.sleep(t.backoff(try-1)) {
				return
			}
		}
	}
	t.debugf("%s: gave up after %d tries", l.name, maxTries)
}

func (t *transport) kill(l *lane, status int) {
	l.mu.Lock()
	l.dead = true
	l.queue, l.bytes = nil, 0
	l.mu.Unlock()
	t.debugf("%s: %d, the key was refused; this endpoint stops until restart", l.name, status)
}

func (t *transport) do(o *outbound) (*http.Response, error) {
	req, err := http.NewRequestWithContext(t.ctx, http.MethodPost, o.url, bytes.NewReader(o.body))
	if err != nil {
		return nil, err
	}
	for k, v := range o.header {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", sdkName+"/"+Version)
	return t.httpc.Do(req)
}

// scaled converts a wait in real seconds to the transport's time unit.
func (t *transport) scaled(d time.Duration) time.Duration {
	return time.Duration(d.Seconds() * float64(t.unit))
}

// block applies a 429 seen outside the queue (flags).
func (t *transport) block(name string, h http.Header) {
	l := t.lane(name)
	l.mu.Lock()
	l.blockedUntil = time.Now().Add(t.scaled(retryAfter(h)))
	l.mu.Unlock()
}

// flush waits until every lane is idle, or the timeout passes.
func (t *transport) flush(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if t.idle() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (t *transport) idle() bool {
	t.mu.Lock()
	lanes := make([]*lane, 0, len(t.lanes))
	for _, l := range t.lanes {
		lanes = append(lanes, l)
	}
	t.mu.Unlock()
	for _, l := range lanes {
		l.mu.Lock()
		busy := l.running || len(l.queue) > 0
		l.mu.Unlock()
		if busy {
			return false
		}
	}
	return true
}

func (t *transport) close() {
	t.cancel()
	t.wg.Wait()
}

// retryAfter reads Retry-After, then X-Sentry-Rate-Limits: at least 1 s, at
// most 1 h, 60 s when neither says. The result is in seconds-as-Duration.
func retryAfter(h http.Header) time.Duration {
	secs := 0.0
	if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get("Retry-After")), 64); err == nil && v > 0 {
		secs = v
	} else if when, err := http.ParseTime(h.Get("Retry-After")); err == nil {
		secs = time.Until(when).Seconds()
	}
	if secs <= 0 {
		if rl := h.Get("X-Sentry-Rate-Limits"); rl != "" {
			first := strings.SplitN(strings.SplitN(rl, ",", 2)[0], ":", 2)[0]
			if v, err := strconv.ParseFloat(strings.TrimSpace(first), 64); err == nil && v > 0 {
				secs = v
			}
		}
	}
	if secs <= 0 {
		secs = 60
	}
	secs = min(3600, max(1, secs))
	return time.Duration(secs * float64(time.Second))
}
