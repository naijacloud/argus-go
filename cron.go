package argus

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// CronMonitor reports cron runs to Argus cron monitors, by the monitor's
// slug. Create the monitor in Argus first: unknown slugs are refused (404).
// Use the package's Cron value.
type CronMonitor struct{}

// Cron is the package's CronMonitor.
var Cron CronMonitor

const maxCronOutput = 2048

// CheckIn runs job and reports it: a start, then success, or failure with
// the error's message. It returns job's error. A panic in job is reported as
// a failure and carries on.
//
//	err := argus.Cron.CheckIn(ctx, "nightly-invoices", sendInvoices)
//
// In a program that exits after the job, call argus.Flush before exiting.
func (m CronMonitor) CheckIn(ctx context.Context, slug string, job func(ctx context.Context) error) (err error) {
	ctx = orBackground(ctx)
	m.Start(ctx, slug)
	defer func() {
		if v := recover(); v != nil {
			m.Fail(ctx, slug, fmt.Sprintf("panic: %v", v))
			panic(v)
		}
		if err != nil {
			m.Fail(ctx, slug, err.Error())
		} else {
			m.Success(ctx, slug)
		}
	}()
	if job == nil {
		return nil
	}
	return job(ctx)
}

// Start opens a run.
func (CronMonitor) Start(ctx context.Context, slug string) { checkIn(slug, "/start", "") }

// Success closes the open run as a success.
func (CronMonitor) Success(ctx context.Context, slug string) { checkIn(slug, "", "") }

// Fail closes the open run as failed. output (the first 2 KB, scrubbed) is
// shown on the run.
func (CronMonitor) Fail(ctx context.Context, slug, output string) { checkIn(slug, "/fail", output) }

// Exit closes the open run with a process exit code: 0 is a success, anything
// else a failure.
func (CronMonitor) Exit(ctx context.Context, slug string, code int, output string) {
	checkIn(slug, "/"+strconv.Itoa(code), output)
}

func checkIn(slug, suffix, output string) {
	c := get()
	if c == nil || slug == "" {
		return
	}
	defer c.guard()
	body := []byte(truncate(scrubString(output), maxCronOutput))
	if len(body) > maxCronOutput {
		body = body[:maxCronOutput]
	}
	c.tr.send(laneCrons, &outbound{
		url:  c.dsn.url("/v1/cron/" + url.PathEscape(slug) + suffix),
		body: body,
		header: map[string][]string{
			"Content-Type":  {"text/plain; charset=utf-8"},
			"Authorization": {"Bearer " + c.dsn.publicKey},
		},
	})
}
