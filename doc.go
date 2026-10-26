// Package argus is the official Go SDK for Naijacloud Argus: errors, logs,
// metrics, traces, cron check-ins and feature flags, with one key, the
// source's DSN. It implements the Argus SDK contract argus.sdk/1 and uses
// only the standard library.
//
// # Setup
//
//	import argus "github.com/naijacloud/argus-go"
//
//	func main() {
//		argus.Init(argus.Options{DSN: "https://<key>@argus-in.naijacloud.com/<id>"})
//		defer argus.Close(2 * time.Second)
//		defer argus.Recover()
//		…
//	}
//
// Without a DSN (the option, ARGUS_DSN or SENTRY_DSN) every function is a
// no-op. Nothing in this package panics or blocks the app: sends are queued
// and made in the background, and failures are dropped (set Options.Debug to
// see them).
//
// # Context first
//
// Every call takes a context.Context first. It carries the scope (tags, the
// user and breadcrumbs, see WithScope) and the active span, so errors and logs
// are tied to the request they happened in:
//
//	argus.CaptureException(ctx, err)
//	argus.Log.Info(ctx, "order placed", "orderId", id)
//	argus.Metrics.Count(ctx, "orders.placed", 1)
//	ctx, span := argus.StartSpan(ctx, "charge card")
//	argus.Cron.CheckIn(ctx, "nightly-invoices", sendInvoices)
//	argus.Flags.IsOn(ctx, "new-checkout", userID)
//
// # HTTP
//
// Middleware traces incoming requests (W3C traceparent, http.route from Go
// 1.23 ServeMux patterns) and reports panics. RoundTripper traces outgoing
// requests and forwards traceparent.
//
// # Privacy
//
// Sensitive keys (password, token, authorization, cookie, bvn, card and the
// rest of the contract's list) are replaced with "[Filtered]" at any depth,
// card numbers, bearer tokens and JWTs are filtered inside strings, query
// strings are removed from URLs, cookies and request bodies are never sent,
// and the user's IP address is only sent with Options.SendDefaultPii.
package argus
