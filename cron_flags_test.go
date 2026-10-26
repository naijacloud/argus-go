package argus

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCronCheckIn(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	ctx := context.Background()
	if err := Cron.CheckIn(ctx, "nightly-invoices", func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	jobErr := errors.New("smtp refused: password=hunter2")
	if err := Cron.CheckIn(ctx, "nightly-invoices", func(context.Context) error { return jobErr }); err != jobErr {
		t.Fatal("CheckIn returns the job's error")
	}
	Cron.Exit(ctx, "backup job", 3, "")
	Flush(time.Second)
	hits := s.waitFor(t, "/v1/cron/", 5)
	want := []string{
		"/v1/cron/nightly-invoices/start",
		"/v1/cron/nightly-invoices",
		"/v1/cron/nightly-invoices/start",
		"/v1/cron/nightly-invoices/fail",
		"/v1/cron/backup%20job/3",
	}
	for i, w := range want {
		got := hits[i].Path
		if i == 4 {
			got = strings.Replace(got, " ", "%20", 1)
		}
		if got != w {
			t.Errorf("check-in %d: %s, want %s", i, got, w)
		}
		if hits[i].Header.Get("Authorization") != "Bearer "+testKey {
			t.Error("cron auth")
		}
	}
	if body := string(hits[3].Body); body != "smtp refused: password="+Filtered {
		t.Errorf("fail output %q", body)
	}
}

func TestCronUnknownSlugDoesNotStopTheEndpoint(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply {
		if strings.Contains(h.Path, "missing") {
			return reply{status: 404}
		}
		return reply{}
	})
	start(t, s, Options{})
	Cron.Success(context.Background(), "missing")
	Cron.Success(context.Background(), "present")
	Flush(time.Second)
	if n := len(s.byPath("/v1/cron/")); n != 2 {
		t.Fatalf("a 404 drops one check-in only; got %d requests", n)
	}
}

func TestCronPanic(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	func() {
		defer func() {
			if recover() != "disk full" {
				t.Error("the panic carries on")
			}
		}()
		_ = Cron.CheckIn(context.Background(), "job", func(context.Context) error { panic("disk full") })
	}()
	Flush(time.Second)
	hits := s.waitFor(t, "/v1/cron/", 2)
	if hits[1].Path != "/v1/cron/job/fail" || string(hits[1].Body) != "panic: disk full" {
		t.Errorf("%s %q", hits[1].Path, hits[1].Body)
	}
}

func TestCronOutputCapped(t *testing.T) {
	s := newStub(t, nil)
	start(t, s, Options{})
	Cron.Fail(context.Background(), "job", strings.Repeat("é", 3000))
	Flush(time.Second)
	if n := len(s.waitFor(t, "/v1/cron/", 1)[0].Body); n > maxCronOutput {
		t.Fatalf("output %d bytes", n)
	}
}

func TestFlags(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply {
		return reply{body: `{"flags":{"new-checkout":"on","old-nav":"off","theme":"dark"},"environment":"test"}`}
	})
	start(t, s, Options{Environment: "test"})
	ctx := context.Background()
	attrs := FlagAttributes(map[string]string{"plan": "pro"})
	if !Flags.IsOn(ctx, "new-checkout", "user-1", attrs) {
		t.Error("new-checkout is on")
	}
	if Flags.IsOn(ctx, "old-nav", "user-1", attrs, FlagDefault(true)) {
		t.Error("old-nav is off, whatever the default")
	}
	if Flags.IsOn(ctx, "unknown", "user-1", attrs) || !Flags.IsOn(ctx, "unknown", "user-1", attrs, FlagDefault(true)) {
		t.Error("unknown flags answer the default")
	}
	if Flags.Variant(ctx, "theme", "user-1", "light", attrs) != "dark" || Flags.Variant(ctx, "nope", "user-1", "light", attrs) != "light" {
		t.Error("variant")
	}
	if all := Flags.All(ctx, "user-1", attrs); len(all) != 3 {
		t.Errorf("all %v", all)
	}
	hits := s.byPath("/v1/flags/evaluate")
	if len(hits) != 1 {
		t.Fatalf("answers are cached per subject; got %d requests", len(hits))
	}
	if hits[0].Header.Get("Authorization") != "Bearer "+testKey || hits[0].Header.Get("Content-Type") != "application/json" {
		t.Error("flag headers")
	}
	var body map[string]any
	_ = json.Unmarshal(hits[0].Body, &body)
	if body["subject"] != "user-1" || body["environment"] != "test" || path(t, body, "attributes", "plan") != "pro" {
		t.Errorf("request body %v", body)
	}
	Flags.IsOn(ctx, "new-checkout", "user-2")
	if n := len(s.byPath("/v1/flags/evaluate")); n != 2 {
		t.Errorf("another subject is another request; got %d", n)
	}
}

func TestFlagsFailOpenToTheDefault(t *testing.T) {
	s := newStub(t, func(h hit, n int) reply {
		switch n {
		case 0:
			return reply{status: 500}
		case 1:
			return reply{delay: 300 * time.Millisecond, body: `{"flags":{"f":"on"}}`}
		default:
			return reply{status: 401}
		}
	})
	start(t, s, Options{FlagTimeout: 50 * time.Millisecond})
	ctx := context.Background()
	if Flags.IsOn(ctx, "f", "a") {
		t.Error("500 answers the default")
	}
	began := time.Now()
	if Flags.IsOn(ctx, "f", "b") {
		t.Error("a timeout answers the default")
	}
	if time.Since(began) > 250*time.Millisecond {
		t.Error("FlagTimeout must bound the wait")
	}
	if !Flags.IsOn(ctx, "f", "c", FlagDefault(true)) {
		t.Error("401 answers the default")
	}
	Flags.IsOn(ctx, "f", "d")
	if n := len(s.byPath("/v1/flags/")); n != 3 {
		t.Errorf("after 401 flags stop asking; got %d requests", n)
	}
	ctx2, cancel := context.WithCancel(ctx)
	cancel()
	Flags.IsOn(ctx2, "f", "e")
}
