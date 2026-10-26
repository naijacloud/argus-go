<p align="center">
  <a href="https://www.naijacloud.com/argus">
    <img alt="Naijacloud Argus, Go SDK" src="https://raw.githubusercontent.com/naijacloud/argus-go/main/.github/assets/banner.png" width="100%">
  </a>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/naijacloud/argus-go"><img alt="go" src="https://img.shields.io/badge/go-argus--go-008751?style=flat-square&labelColor=0A0E0C"></a>
  <img alt="go version" src="https://img.shields.io/badge/go-%3E%3D_1.22-00ADD8?style=flat-square&labelColor=0A0E0C">
  <img alt="dependencies" src="https://img.shields.io/badge/dependencies-0-46C98A?style=flat-square&labelColor=0A0E0C">
  <a href="LICENSE"><img alt="license" src="https://img.shields.io/badge/license-MIT-8A988F?style=flat-square&labelColor=0A0E0C"></a>
</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#errors-and-panics">Errors</a> ·
  <a href="#nethttp">net/http</a> ·
  <a href="#logs">Logs</a> ·
  <a href="#metrics">Metrics</a> ·
  <a href="#traces">Traces</a> ·
  <a href="#cron-check-ins">Crons</a> ·
  <a href="#feature-flags">Flags</a> ·
  <a href="#privacy">Privacy</a>
</p>

# argus-go

The official Go SDK for [Argus](https://www.naijacloud.com/argus),
Naijacloud's monitoring. One package sends errors, logs, metrics, traces, cron
check-ins and feature-flag checks, with one key: your app's **DSN**.

Version 0.1.0. It implements the Argus SDK contract **`argus.sdk/1`**, uses
only the standard library, needs Go 1.22 or newer, and works for apps hosted
anywhere, not only on Naijacloud.

**Already using `sentry-go`?** Keep it. Argus accepts it: change the DSN and
you're done. Switch to this SDK when you want logs, metrics, cron check-ins
and flags through the same setup.

## Install

```sh
go get github.com/naijacloud/argus-go
```

```go
import argus "github.com/naijacloud/argus-go"

func main() {
	argus.Init(argus.Options{DSN: "https://<key>@argus-in.naijacloud.com/<id>"})
	defer argus.Close(2 * time.Second) // send what's queued before exiting
	defer argus.Recover()              // report a crash, then crash as before
	// …
}
```

Copy the DSN from the source in Argus. Leave `DSN` empty to read
`ARGUS_DSN`, then `SENTRY_DSN`. Apps hosted on Naijacloud get `ARGUS_DSN`,
`ARGUS_RELEASE` and `ARGUS_ENVIRONMENT` set when the Argus tab is on.

**Without a DSN the SDK does nothing,** so the same code runs locally and in
tests. Nothing in this package panics into your code or blocks it: everything
is queued and sent in the background.

**Context comes first on every call.** It carries the request's scope (tags,
user, breadcrumbs) and the active span, so an error or a log line is tied to
the request it happened in.

## Errors and panics

```go
if err := chargeCard(ctx, order); err != nil {
	argus.CaptureException(ctx, err, argus.Details{
		Tags:  map[string]string{"provider": "paystack"},
		Extra: map[string]any{"orderId": order.ID},
	})
}

argus.CaptureMessage(ctx, "webhook signature mismatch", argus.LevelWarning)
argus.SetUser(ctx, argus.User{ID: user.ID})
argus.SetTag(ctx, "region", "af-west")
argus.AddBreadcrumb(ctx, argus.Breadcrumb{Category: "auth", Message: "signed in"})
```

- **Causes:** the whole `errors.Unwrap` chain is sent (including
  `errors.Join` and multiple `%w`), with the stack where you captured it.
  Errors from `github.com/pkg/errors` keep the stack they recorded.
- **Stack frames** carry the function, package, file and line, and source
  lines when the source is on disk. Your code is marked in-app; the standard
  library and modules from the module cache are not.
- **Scopes:** `argus.WithScope(ctx)` gives a job or goroutine its own tags,
  user and breadcrumbs. The middleware does it for every request. Calls with a
  context that has no scope set things for the whole process.
- **Panics:** `defer argus.Recover()` in `main` and at the top of each
  goroutine reports the panic, flushes, and panics again, so your program fails
  the way it would have. Use `defer argus.RecoverWithContext(ctx)` to keep the
  request's scope. To carry on after a panic, recover it yourself and call
  `argus.CapturePanic(ctx, v)`.
- **`BeforeSend`** lets you change an event or return `nil` to drop it.

## net/http

```go
mux := http.NewServeMux()
mux.HandleFunc("GET /orders/{id}", getOrder)

http.ListenAndServe(":8080", argus.Middleware(mux))
```

For every request, `Middleware`:

- **traces it:** a server span named `GET /orders/{id}`, with
  `http.request.method`, `http.route`, `http.response.status_code` and
  `url.full` (no query string). A `traceparent` header from the caller
  continues its trace. `http.route` comes from the Go 1.23+ `ServeMux`
  pattern; with another router, set it from your handler with
  `argus.SpanFromContext(ctx).SetAttribute("http.route", "/orders/:id")`.
- **scopes it:** tags, the user and breadcrumbs set with `r.Context()` stay on
  that request's events, which also carry the method, URL and headers (never
  cookies or the body).
- **reports panics,** then panics again so `net/http` handles them as before.

For outgoing calls, wrap your client's transport. Requests made inside a span
become child spans, carry `traceparent`, and leave breadcrumbs:

```go
client := &http.Client{Transport: argus.RoundTripper(nil)} // nil: http.DefaultTransport
req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.paystack.co/bank", nil)
res, err := client.Do(req)
```

The SDK's own requests to Argus never go through your transports.

## Logs

```go
argus.Log.Info(ctx, "order placed", "orderId", "ord_81", "total", 18500)
argus.Log.Error(ctx, "paystack timed out", "attempt", 3)
```

Arguments are slog-style: alternating keys and values, or `slog.Attr`. Logs
are sent in batches every 5 seconds (or 1,000 records), and a log written
inside a span carries its trace id.

**Already on `log/slog`?** Send its records to Argus and keep your local
output:

```go
slog.SetDefault(slog.New(argus.NewSlogHandler(&argus.SlogOptions{
	Next:          slog.NewJSONHandler(os.Stdout, nil), // still printed
	CaptureErrors: true,                                // Error records also become events
})))

slog.InfoContext(ctx, "order placed", "orderId", id) // use the *Context forms
```

Records at `slog.LevelInfo` and above go to Argus (change it with `Level`),
and each one is a breadcrumb for the next error.

## Metrics

```go
argus.Metrics.Count(ctx, "orders.placed", 1, "method", "card")
argus.Metrics.Gauge(ctx, "queue.depth", float64(len(jobs)))
argus.Metrics.Histogram(ctx, "checkout.duration", ms, argus.Unit("ms"))
```

Metrics are added up in your process and sent every 10 seconds. Counters and
histograms are sent as deltas. Every distinct set of attributes is its own
series, so keep attribute values few (a user id is not a good attribute).

## Traces

```go
err := argus.Trace(ctx, "send invoices", func(ctx context.Context) error {
	return sendInvoices(ctx)
})

ctx, span := argus.StartSpan(ctx, "charge card", "provider", "paystack")
defer span.End()
span.SetError(err)
```

- **Spans** join the active span in `ctx`, or start a trace.
- **`argus.ContinueTrace(ctx, traceparent)`** continues a trace from another
  service, such as a queue message.
- **`span.Traceparent()`** is the header for calls you make yourself.
- **Sample traces** with `TracesSampleRate` (0 to 1), or turn them off with
  `DisableTracing`. A nil `*Span` is valid, so code that uses spans needs no
  checks when the SDK is off.

## Cron check-ins

Create a cron monitor in Argus first, then report runs by its slug:

```go
err := argus.Cron.CheckIn(ctx, "nightly-invoices", sendInvoices)
```

`CheckIn` reports the start, then success, or failure with the error's
message, and returns your job's error. A panic in the job is reported as a
failure and carries on. By hand:

```go
argus.Cron.Start(ctx, "nightly-invoices")
argus.Cron.Success(ctx, "nightly-invoices") // or Fail(ctx, slug, output), Exit(ctx, slug, code, output)
```

A program that exits right after its job must call `argus.Close` (or
`argus.Flush`) first, or the last check-in may not leave.

## Feature flags

```go
if argus.Flags.IsOn(ctx, "new-checkout", user.ID) {
	// …
}
theme := argus.Flags.Variant(ctx, "theme", user.ID, "light")
on := argus.Flags.IsOn(ctx, "beta", user.ID,
	argus.FlagAttributes(map[string]string{"plan": "pro"}),
	argus.FlagDefault(true))
```

- **Caching:** answers are cached for 30 seconds per subject.
- **Fallback:** if Argus can't be reached within `FlagTimeout` (2 s), or the
  flag doesn't exist, you get the default: `false` for `IsOn` unless you pass
  `FlagDefault(true)`, and the fallback you passed for `Variant`.
- Flag calls are the one place the SDK waits for Argus, because the answer is
  needed. Pass a context with a deadline to bound them further.

## Privacy

On by default:

- The values of keys such as `password`, `token`, `secret`, `authorization`,
  `cookie`, `bvn`, `pin` and `card` are replaced with `[Filtered]`, at any
  depth, in events, breadcrumbs, log and span attributes.
- Card numbers (checked with Luhn), bearer and basic credentials, JWTs and
  `password=…` pairs are filtered inside strings.
- Query strings and `user:password@` are removed from URLs in events,
  breadcrumbs and spans.
- Cookies and request bodies are never sent.
- The user's IP address is not sent unless you set `SendDefaultPii: true`.

Argus filters again on arrival.

## Options

| Option | Default | |
| -- | -- | -- |
| `DSN` | `ARGUS_DSN`, then `SENTRY_DSN` | Without one, nothing is sent |
| `Release` | `ARGUS_RELEASE`, then `SENTRY_RELEASE` | Your app's version, like `1.4.2` or a commit |
| `Environment` | `ARGUS_ENVIRONMENT`, `SENTRY_ENVIRONMENT`, else `production` | |
| `ServiceName` | `ARGUS_SERVICE_NAME`, else your main module's name | Shown on logs, metrics and traces |
| `ServerName` | the host name | |
| `SampleRate` | `1` | Share of errors sent |
| `TracesSampleRate` | `1` | Share of traces recorded |
| `DisableTracing` | `false` | Record no spans (traceparent is still forwarded) |
| `SendDefaultPii` | `false` | Send the user's IP address |
| `MaxBreadcrumbs` | `100` | Per scope; Argus keeps at most 100 |
| `BeforeSend` / `BeforeBreadcrumb` | | Change or drop an event or a breadcrumb |
| `Debug` / `DebugWriter` | `false` / `os.Stderr` | Print what the SDK does (or `ARGUS_DEBUG=1`) |
| `Disabled` | `false` | Turn the SDK off without removing it |
| `HTTPClient` | its own, 30 s timeout | The client used to reach Argus |
| `FlagTimeout` | `2s` | Bound on one flag evaluation |

## Delivery

- Every signal has its own queue (at most 100 requests and 1 MB); when one is
  full the oldest request is dropped. Batches are split to stay under the
  ingest size limits.
- Network errors and `5xx` answers are retried with backoff (1 s, 2 s, 4 s …,
  at most 5 tries). A `429` pauses that signal for `Retry-After`, then the
  request is sent again. A `401` or `403` (a revoked key) stops that signal
  until the process restarts. Other `4xx` answers are dropped.
- `argus.Flush(timeout)` waits for the queues to empty and reports whether
  they did; `argus.Close(timeout)` flushes and stops.

## Examples

- [`examples/http`](examples/http): a `net/http` service with the middleware,
  an outgoing call, `log/slog`, a metric and a flag.
- [`examples/cron`](examples/cron): a scheduled job reporting to a cron monitor.

## Not in this version

- automatic spans for database clients (`database/sql`) and gRPC;
- framework adapters (Gin, Echo, Fiber, chi). They work through
  `argus.Middleware` where they accept `net/http` middleware.

## License

MIT
