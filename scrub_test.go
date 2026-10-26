package argus

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSensitiveKeys(t *testing.T) {
	for _, k := range []string{"password", "Password", "db_password", "X-Api-Key", "apiKey", "access_key", "Authorization",
		"refreshToken", "client-secret", "cookie", "Set-Cookie", "private key", "card_number", "creditCard", "bvn", "cvv",
		"cvc", "sessionId", "session_token", "signature", "auth", "pwd", "pin", "otp", "session", "card", "sid", "credentials"} {
		if !sensitiveKey(k) {
			t.Errorf("%q should be sensitive", k)
		}
	}
	for _, k := range []string{"orderId", "email", "author", "pinned", "user", "id", "spin", "side"} {
		if sensitiveKey(k) {
			t.Errorf("%q should not be sensitive", k)
		}
	}
}

func TestScrubString(t *testing.T) {
	cases := map[string]string{
		"card 4242424242424242 declined":                                "card " + Filtered + " declined",
		"card 4242-4242-4242-4242":                                      "card " + Filtered,
		"order 1234567890123 (fails Luhn)":                              "order 1234567890123 (fails Luhn)",
		"Authorization: Bearer abc.def-ghi_jkl":                         "Authorization: Bearer " + Filtered,
		"header Bearer abcdefghijkl ok":                                 "header Bearer " + Filtered + " ok",
		"Basic dXNlcjpwYXNzd29yZA==":                                    "Basic " + Filtered,
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlX19f end": "jwt " + Filtered + " end",
		"login password=hunter2&user=bob":                               "login password=" + Filtered + "&user=bob",
		`{"pin": "1234"}`:                                               `{"pin": "` + Filtered + `"}`,
		"api_key: sk_live_123, region: lagos":                           "api_key: " + Filtered + ", region: lagos",
		"Error: timeout":                                                "Error: timeout",
		"nothing here":                                                  "nothing here",
	}
	for in, want := range cases {
		if got := scrubString(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}

func TestStripQuery(t *testing.T) {
	cases := map[string]string{
		"https://u:p@api.example.com/a/b?token=1#frag": "https://api.example.com/a/b",
		"/orders/7?x=1":       "/orders/7",
		"https://example.com": "https://example.com",
	}
	for in, want := range cases {
		if got := stripQuery(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestScrubDeep(t *testing.T) {
	v := scrubValue(map[string]any{
		"user": map[string]any{"name": "Ada", "Password": "x", "tokens": []any{"a"}},
		"list": []any{map[string]any{"secret": "s"}, "Bearer abcdefghijk"},
		"pin":  "",
	}, 0).(map[string]any)
	if path(t, v, "user", "Password") != Filtered || path(t, v, "user", "tokens") != Filtered || path(t, v, "user", "name") != "Ada" {
		t.Errorf("%v", v)
	}
	if path(t, v, "list", 0, "secret") != Filtered || path(t, v, "list", 1) != "Bearer "+Filtered {
		t.Errorf("%v", v)
	}
	if v["pin"] != "" {
		t.Error("empty values stay empty")
	}
}

func TestSlogHandler(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	var local bytes.Buffer
	h := NewSlogHandler(&SlogOptions{Next: slog.NewTextHandler(&local, &slog.HandlerOptions{Level: slog.LevelDebug}), CaptureErrors: true})
	logger := slog.New(h).With("svc", "api").WithGroup("req")
	ctx := WithScope(context.Background())
	logger.DebugContext(ctx, "not sent to Argus")
	logger.InfoContext(ctx, "handled", "id", 7, "password", "hunter2")
	logger.ErrorContext(ctx, "charge failed", "err", errors.New("card declined"))
	if !strings.Contains(local.String(), "not sent to Argus") || !strings.Contains(local.String(), "charge failed") {
		t.Errorf("Next must see every record: %q", local.String())
	}
	Flush(time.Second)
	recs := path(t, decode(t, s.waitFor(t, "/v1/logs", 1)[0].Body), "resourceLogs", 0, "scopeLogs", 0, "logRecords").([]any)
	if len(recs) != 2 {
		t.Fatalf("records %d (debug is below the default level)", len(recs))
	}
	a := otlpAttrs(t, path(t, recs[0], "attributes"))
	if a["svc"]["stringValue"] != "api" || a["req.id"]["intValue"] != "7" || a["req.password"]["stringValue"] != Filtered {
		t.Errorf("attributes %v", a)
	}
	if path(t, recs[1], "severityNumber") != 17.0 || path(t, recs[1], "severityText") != "ERROR" {
		t.Error("error severity")
	}
	ev := parseEnvelope(t, s.waitFor(t, "/api/", 1)[0].Body).Event
	if path(t, ev, "exception", "values", 0, "value") != "card declined" || path(t, ev, "extra", "log.message") != "charge failed" {
		t.Errorf("event %v", ev)
	}
	crumbs := path(t, ev, "breadcrumbs", "values").([]any)
	if len(crumbs) < 1 || path(t, crumbs[0], "message") != "handled" || path(t, crumbs[0], "category") != "log" {
		t.Errorf("breadcrumbs %v", crumbs)
	}
	if !h.Enabled(ctx, slog.LevelDebug) || NewSlogHandler(nil).Enabled(ctx, slog.LevelDebug) {
		t.Error("Enabled follows the lowest of Argus's level and Next's")
	}
}
