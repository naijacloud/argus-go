# Changelog

All notable changes to `github.com/naijacloud/argus-go` are listed here. The
format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and
the module uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-10-26

First release, implementing the Argus SDK contract `argus.sdk/1`.

### Added

- **Setup:** `argus.Init(argus.Options{DSN: …})`, reading `ARGUS_DSN`,
  `ARGUS_RELEASE` and `ARGUS_ENVIRONMENT` (then the `SENTRY_*` names) when an
  option is empty. Without a DSN every call is a no-op.
- **Errors:** `CaptureException` with the full `errors.Unwrap` chain and the
  capture-site stack (source lines when available; `github.com/pkg/errors`
  stacks kept), `CaptureMessage`, `BeforeSend`, sampling.
- **Panics:** `Recover` and `RecoverWithContext` report, flush and re-panic;
  `CapturePanic` for values you recover yourself. A panic is sent once even
  when several handlers see it.
- **Scopes through `context.Context`:** `WithScope`, `SetUser`, `SetTag`,
  `SetTags`, `SetContext`, `SetExtra`, `AddBreadcrumb` (last 100 kept).
- **net/http:** `Middleware` traces incoming requests (W3C `traceparent`,
  `http.route` from Go 1.23+ `ServeMux` patterns), scopes each request and
  reports panics; `RoundTripper` traces outgoing requests and forwards
  `traceparent`.
- **Logs, metrics and traces** over OTLP/HTTP JSON:
  - `Log.Info()` and the other levels, and `NewSlogHandler` for `log/slog`;
  - `Metrics.Count()`, `Metrics.Gauge()` and `Metrics.Histogram()`, as deltas;
  - `StartSpan()`, `Trace()` and `ContinueTrace()`.
- **Cron check-ins by monitor slug:** `Cron.CheckIn(ctx, slug, job)`, plus
  `Start`, `Success`, `Fail` and `Exit`.
- **Feature flags:** `Flags.IsOn()`, `Flags.Variant()` and `Flags.All()`,
  cached for 30 seconds, falling back to the default.
- **Privacy defaults on:** sensitive keys, card numbers, credentials and JWTs
  are filtered, query strings removed, cookies and bodies never sent, and IP
  addresses only sent with `SendDefaultPii`.
- **Transport:** one bounded queue per endpoint, retries with backoff,
  `Retry-After` on 429, stops on a refused key, never panics or blocks.
  `Flush(timeout)` and `Close(timeout)`.

[Unreleased]: https://github.com/naijacloud/argus-go/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/naijacloud/argus-go/releases/tag/v0.1.0
