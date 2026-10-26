// Command http is a small net/http service instrumented with Argus: traced
// requests, panics reported, logs through log/slog, an outgoing call that
// carries traceparent, a metric and a feature flag.
//
//	ARGUS_DSN=https://<key>@argus-in.naijacloud.com/<id> go run ./examples/http
//	curl localhost:8080/orders/42
//	curl localhost:8080/panic
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	argus "github.com/naijacloud/argus-go"
)

func main() {
	// Reads ARGUS_DSN, ARGUS_RELEASE and ARGUS_ENVIRONMENT. Without a DSN the
	// SDK does nothing, so this runs fine locally too.
	if err := argus.Init(argus.Options{Release: "example@0.1.0"}); err != nil {
		fmt.Fprintln(os.Stderr, "argus:", err)
	}
	defer argus.Close(2 * time.Second)
	defer argus.Recover()

	// Logs go to stdout as before, and to Argus Logs. Errors become events.
	slog.SetDefault(slog.New(argus.NewSlogHandler(&argus.SlogOptions{
		Next:          slog.NewTextHandler(os.Stdout, nil),
		CaptureErrors: true,
	})))

	// Outgoing calls made with this client join the request's trace.
	api := &http.Client{Transport: argus.RoundTripper(nil), Timeout: 10 * time.Second}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := r.PathValue("id")
		argus.SetTag(ctx, "order", id)
		argus.SetUser(ctx, argus.User{ID: "user-" + id})

		if argus.Flags.IsOn(ctx, "new-checkout", "user-"+id) {
			argus.SetTag(ctx, "checkout", "new")
		}

		err := argus.Trace(ctx, "load order", func(ctx context.Context) error {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com/", nil)
			res, err := api.Do(req)
			if err != nil {
				return err
			}
			return res.Body.Close()
		})
		if err != nil {
			slog.ErrorContext(ctx, "order lookup failed", "order", id, "err", err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		argus.Metrics.Count(ctx, "orders.viewed", 1)
		slog.InfoContext(ctx, "order served", "order", id)
		fmt.Fprintf(w, "order %s\n", id)
	})
	mux.HandleFunc("GET /fail", func(w http.ResponseWriter, r *http.Request) {
		argus.CaptureException(r.Context(), errors.New("something went wrong"))
		http.Error(w, "reported", http.StatusInternalServerError)
	})
	mux.HandleFunc("GET /panic", func(http.ResponseWriter, *http.Request) {
		var m map[string]int
		m["boom"]++ // a real nil-map panic, reported by the middleware
	})

	srv := &http.Server{Addr: ":8080", Handler: argus.Middleware(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server stopped", "err", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
