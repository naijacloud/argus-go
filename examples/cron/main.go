// Command cron is a scheduled job that reports each run to an Argus cron
// monitor. Create a monitor with the slug "nightly-invoices" in Argus first.
//
//	ARGUS_DSN=https://<key>@argus-in.naijacloud.com/<id> go run ./examples/cron
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	argus "github.com/naijacloud/argus-go"
)

func sendInvoices(ctx context.Context) error {
	argus.Log.Info(ctx, "sending invoices", "count", 3)
	time.Sleep(100 * time.Millisecond)
	argus.Metrics.Count(ctx, "invoices.sent", 3)
	return nil
}

func main() {
	if err := argus.Init(argus.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, "argus:", err)
	}
	ctx := context.Background()

	// Reports a start, then success, or failure with the error.
	err := argus.Cron.CheckIn(ctx, "nightly-invoices", sendInvoices)
	if err != nil {
		argus.CaptureException(ctx, err)
	}

	// A job that exits right after must flush, or the check-in may not leave.
	argus.Close(5 * time.Second)
	if err != nil {
		os.Exit(1)
	}
}
