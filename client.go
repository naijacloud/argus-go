package argus

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is the SDK version. It is sent as sdk.version on errors and as
// telemetry.sdk.version on logs, metrics and traces.
const Version = "0.1.0"

// Contract is the Argus SDK wire contract this package implements.
const Contract = "argus.sdk/1"

// sdkName is how Argus tells this SDK from Sentry's (spec §4).
const sdkName = "argus.go"

const (
	defaultFlushTimeout = 2 * time.Second
	defaultFlagTimeout  = 2 * time.Second
	flushLogsEvery      = 5 * time.Second
	flushSpansEvery     = 5 * time.Second
	flushMetricsEvery   = 10 * time.Second
	maxLogBatch         = 1000
	maxSpanBatch        = 512
	maxBreadcrumbs      = 100
)

// Options configures the SDK. Every field is optional: with no DSN anywhere
// (the option, ARGUS_DSN or SENTRY_DSN) the SDK sends nothing.
type Options struct {
	// DSN is the source's key, https://<key>@argus-in.naijacloud.com/<id>.
	// Default: ARGUS_DSN, then SENTRY_DSN.
	DSN string
	// Release is the app's version, such as "1.4.2" or a commit.
	// Default: ARGUS_RELEASE, then SENTRY_RELEASE.
	Release string
	// Environment defaults to ARGUS_ENVIRONMENT, then SENTRY_ENVIRONMENT,
	// then "production".
	Environment string
	// ServiceName is service.name on logs, metrics and traces. Default:
	// ARGUS_SERVICE_NAME, then the last element of the main module's path,
	// then the executable's name.
	ServiceName string
	// ServerName defaults to the host name.
	ServerName string
	// SampleRate is the share of errors sent, 0 to 1. Zero means 1 (send all).
	SampleRate float64
	// TracesSampleRate is the share of traces recorded, 0 to 1. Zero means 1.
	// Set DisableTracing to record none.
	TracesSampleRate float64
	// DisableTracing records no spans. traceparent is still forwarded.
	DisableTracing bool
	// SendDefaultPii sends the user's IP address. Off by default.
	SendDefaultPii bool
	// MaxBreadcrumbs kept per scope. Zero means 100, the most Argus keeps.
	MaxBreadcrumbs int
	// BeforeSend may change an event, or return nil to drop it. It runs
	// before scrubbing, so filtered values are still visible to it.
	BeforeSend func(event *Event) *Event
	// BeforeBreadcrumb may change a breadcrumb, or return nil to drop it.
	BeforeBreadcrumb func(crumb *Breadcrumb) *Breadcrumb
	// Debug prints what the SDK does to DebugWriter (default os.Stderr).
	// ARGUS_DEBUG=1 turns it on too.
	Debug       bool
	DebugWriter io.Writer
	// Disabled turns the SDK off without removing it.
	Disabled bool
	// HTTPClient sends to Argus. Default: a client of its own with a 30 s
	// timeout, so the app's http.DefaultClient settings are not touched.
	HTTPClient *http.Client
	// FlagTimeout bounds one flag evaluation request. Default 2 s.
	FlagTimeout time.Duration

	// timeUnit scales backoff and Retry-After waits (tests only).
	timeUnit time.Duration
}

// client is what Init makes. All public functions talk to the current one.
type client struct {
	opts     Options
	dsn      *dsn
	tr       *transport
	httpc    *http.Client
	resource []kv
	debugMu  sync.Mutex

	logMu   sync.Mutex
	logBuf  []logRecord
	spanMu  sync.Mutex
	spanBuf []spanData
	metrics metricStore

	flags flagCache

	kick     chan struct{}
	stop     chan struct{}
	loopDone chan struct{}
	closed   atomic.Bool
	once     sync.Once

	droppedSeriesOnce sync.Once
}

var current atomic.Pointer[client]

// get returns the live client, or nil before Init, without a DSN, or after Close.
func get() *client {
	c := current.Load()
	if c == nil || c.closed.Load() {
		return nil
	}
	return c
}

func env(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// Init starts the SDK. Options win over ARGUS_* and then SENTRY_* environment
// variables. Without a DSN it returns nil and the SDK stays a no-op. An
// invalid DSN returns an error, and the SDK stays a no-op too: nothing in this
// package panics or blocks because of configuration.
//
// Calling Init again replaces the previous client, after flushing it.
func Init(opts Options) error {
	if prev := current.Swap(nil); prev != nil {
		prev.close(time.Second)
	}
	globalScope.Store(newScope(nil))

	if opts.DSN == "" {
		opts.DSN = env("ARGUS_DSN", "SENTRY_DSN")
	}
	if opts.Release == "" {
		opts.Release = env("ARGUS_RELEASE", "SENTRY_RELEASE")
	}
	if opts.Environment == "" {
		opts.Environment = env("ARGUS_ENVIRONMENT", "SENTRY_ENVIRONMENT")
	}
	if opts.Environment == "" {
		opts.Environment = "production"
	}
	if opts.ServiceName == "" {
		opts.ServiceName = env("ARGUS_SERVICE_NAME")
	}
	if opts.ServiceName == "" {
		opts.ServiceName = defaultServiceName()
	}
	if opts.ServerName == "" {
		opts.ServerName, _ = os.Hostname()
	}
	if !opts.Debug {
		switch strings.ToLower(env("ARGUS_DEBUG")) {
		case "1", "true", "yes", "on":
			opts.Debug = true
		}
	}
	if opts.DebugWriter == nil {
		opts.DebugWriter = os.Stderr
	}
	if opts.SampleRate <= 0 || opts.SampleRate > 1 {
		opts.SampleRate = 1
	}
	if opts.TracesSampleRate <= 0 || opts.TracesSampleRate > 1 {
		opts.TracesSampleRate = 1
	}
	if opts.MaxBreadcrumbs <= 0 || opts.MaxBreadcrumbs > maxBreadcrumbs {
		opts.MaxBreadcrumbs = maxBreadcrumbs
	}
	if opts.FlagTimeout <= 0 {
		opts.FlagTimeout = defaultFlagTimeout
	}
	if opts.timeUnit <= 0 {
		opts.timeUnit = time.Second
	}
	if opts.Disabled || opts.DSN == "" {
		return nil
	}
	d, err := parseDSN(opts.DSN)
	if err != nil {
		if opts.Debug {
			fmt.Fprintf(opts.DebugWriter, "[argus] %v; nothing will be sent\n", err)
		}
		return err
	}

	c := &client{
		opts:     opts,
		dsn:      d,
		kick:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		loopDone: make(chan struct{}),
	}
	c.httpc = opts.HTTPClient
	if c.httpc == nil {
		c.httpc = &http.Client{Timeout: 30 * time.Second, Transport: ownTransport()}
	}
	c.tr = newTransport(c.httpc, opts.timeUnit, c.debugf)
	c.tr.onSuccess = c.onSuccess
	c.resource = attrsToKV([]attr{
		{"service.name", opts.ServiceName},
		{"service.version", opts.Release},
		{"deployment.environment", opts.Environment},
		{"telemetry.sdk.name", sdkName},
		{"telemetry.sdk.language", "go"},
		{"telemetry.sdk.version", Version},
		{"argus.contract", Contract},
	})
	c.metrics.start = time.Now()
	go c.loop()
	current.Store(c)
	c.debugf("started: source %s, environment %s, release %q", d.projectID, opts.Environment, opts.Release)
	return nil
}

// ownTransport is a copy of the default transport, so an app that wraps
// http.DefaultTransport with RoundTripper does not trace the SDK's own sends.
func ownTransport() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, IdleConnTimeout: 90 * time.Second}
}

func defaultServiceName() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		p := bi.Main.Path
		if p != "" && p != "command-line-arguments" {
			return p[strings.LastIndexByte(p, '/')+1:]
		}
	}
	if len(os.Args) > 0 && os.Args[0] != "" {
		return strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	}
	return "app"
}

func (c *client) debugf(format string, args ...any) {
	if c == nil || !c.opts.Debug {
		return
	}
	c.debugMu.Lock()
	defer c.debugMu.Unlock()
	fmt.Fprintf(c.opts.DebugWriter, "[argus] "+format+"\n", args...)
}

// guard swallows a panic from SDK code: the SDK never takes the app down.
func (c *client) guard() {
	if r := recover(); r != nil {
		c.debugf("internal error (dropped): %v", r)
	}
}

func (c *client) sample(rate float64) bool {
	return rate >= 1 || rand.Float64() < rate
}

func (c *client) loop() {
	defer close(c.loopDone)
	logs := time.NewTicker(flushLogsEvery)
	spans := time.NewTicker(flushSpansEvery)
	metrics := time.NewTicker(flushMetricsEvery)
	defer logs.Stop()
	defer spans.Stop()
	defer metrics.Stop()
	for {
		select {
		case <-logs.C:
			c.flushLogs()
		case <-spans.C:
			c.flushSpans()
		case <-metrics.C:
			c.flushMetrics()
		case <-c.kick:
			c.flushLogs()
			c.flushSpans()
		case <-c.stop:
			return
		}
	}
}

func (c *client) poke() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *client) onSuccess(lane string, res *http.Response) {
	if lane == laneMetrics && res.Header.Get("X-Argus-Dropped-Series") != "" {
		c.droppedSeriesOnce.Do(func() {
			c.debugf("metrics: the workspace hit its series limit (X-Argus-Dropped-Series: %s); new series are dropped", res.Header.Get("X-Argus-Dropped-Series"))
		})
	}
}

func (c *client) flush(timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = defaultFlushTimeout
	}
	func() {
		defer c.guard()
		c.flushLogs()
		c.flushSpans()
		c.flushMetrics()
	}()
	return c.tr.flush(timeout)
}

func (c *client) close(timeout time.Duration) bool {
	ok := true
	c.once.Do(func() {
		ok = c.flush(timeout)
		c.closed.Store(true)
		close(c.stop)
		<-c.loopDone
		c.tr.close()
	})
	return ok
}

// Flush sends everything queued and waits up to timeout (default 2 s when
// timeout is zero). It reports whether everything went out in time. Call it
// before a short-lived program exits; long-running servers don't need to.
func Flush(timeout time.Duration) bool {
	c := get()
	if c == nil {
		return true
	}
	return c.flush(timeout)
}

// Close flushes like Flush, then stops the SDK. Later calls are no-ops until
// the next Init.
//
//	defer argus.Close(2 * time.Second)
func Close(timeout time.Duration) bool {
	c := current.Swap(nil)
	if c == nil {
		return true
	}
	return c.close(timeout)
}

// Enabled reports whether Init found a DSN and the SDK is sending.
func Enabled() bool { return get() != nil }

func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// internalKey marks the SDK's own requests, so RoundTripper never traces them.
type internalKey struct{}

func internalContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalKey{}, true)
}

func isInternal(ctx context.Context) bool {
	return ctx != nil && ctx.Value(internalKey{}) != nil
}

func runtimeContexts() map[string]any {
	return map[string]any{
		"runtime": map[string]any{"name": "go", "version": runtime.Version()},
		"os":      map[string]any{"name": runtime.GOOS},
		"device":  map[string]any{"arch": runtime.GOARCH, "num_cpu": runtime.NumCPU()},
	}
}
