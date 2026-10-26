//go:build contract

// Contract test against a real Argus (staging or prod), not a stand-in.
//
//	ARGUS_CONTRACT_DSN=<dsn of a test source> \
//	ARGUS_CONTRACT_API=https://argus-api.naijacloud-staging.com \
//	ARGUS_CONTRACT_TOKEN=<a token that can read that workspace> \
//	go test -tags contract -run TestContract -v .
//
// It sends an error, a log, a span, a metric, a cron check-in and a flag
// evaluation, checks Argus answered 2xx to each, and, when ARGUS_CONTRACT_API
// and _TOKEN are set, waits for the error to show up as an issue. The cron
// check-in expects a monitor with slug argus-go-contract: a 404 is reported,
// not failed, when it is missing.
package argus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingTransport struct {
	base http.RoundTripper
	mu   sync.Mutex
	seen []struct {
		path   string
		status int
	}
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := r.base.RoundTrip(req)
	status := 0
	if err == nil {
		status = res.StatusCode
	}
	r.mu.Lock()
	r.seen = append(r.seen, struct {
		path   string
		status int
	}{req.URL.Path, status})
	r.mu.Unlock()
	return res, err
}

func (r *recordingTransport) statuses(prefix string) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int
	for _, s := range r.seen {
		if strings.HasPrefix(s.path, prefix) {
			out = append(out, s.status)
		}
	}
	return out
}

func TestContract(t *testing.T) {
	dsn := os.Getenv("ARGUS_CONTRACT_DSN")
	if dsn == "" {
		t.Skip("set ARGUS_CONTRACT_DSN to a test source DSN")
	}
	rec := &recordingTransport{base: ownTransport()}
	if err := Init(Options{
		DSN:         dsn,
		Release:     "argus-go@contract",
		Environment: "contract",
		ServiceName: "argus-go-contract",
		HTTPClient:  &http.Client{Transport: rec, Timeout: 30 * time.Second},
		Debug:       os.Getenv("ARGUS_DEBUG") != "",
	}); err != nil {
		t.Fatal(err)
	}

	marker := fmt.Sprintf("argus-go contract %s", randomHex(4))
	ctx := context.Background()
	_ = Trace(ctx, "contract.run", func(ctx context.Context) error {
		var syntax *json.SyntaxError
		err := json.Unmarshal([]byte("{not json"), &struct{}{})
		if !errors.As(err, &syntax) {
			t.Fatalf("want a JSON syntax error, got %v", err)
		}
		CaptureException(ctx, fmt.Errorf("%s: %w", marker, err), Details{Extra: map[string]any{"password": "hunter2"}})
		Log.Info(ctx, "contract log line", "marker", marker)
		return nil
	})
	Metrics.Count(ctx, "argus_go_contract_runs", 1)
	Cron.Success(ctx, "argus-go-contract")
	flag := Flags.IsOn(ctx, "argus-go-contract-flag", "contract-user")
	if !Close(15 * time.Second) {
		t.Fatal("flush timed out")
	}

	ok := func(prefix string) {
		got := rec.statuses(prefix)
		good := len(got) > 0
		for _, s := range got {
			good = good && s >= 200 && s < 300
		}
		if !good {
			t.Errorf("%s: %v", prefix, got)
			return
		}
		t.Logf("ok %s %v", prefix, got)
	}
	ok("/api/")
	ok("/v1/logs")
	ok("/v1/traces")
	ok("/v1/metrics")
	ok("/v1/flags/evaluate")
	if cron := rec.statuses("/v1/cron/"); len(cron) == 1 && cron[0] >= 200 && cron[0] < 300 {
		t.Logf("ok /v1/cron/ %v", cron)
	} else {
		t.Logf("note /v1/cron/ %v (create a cron monitor with slug argus-go-contract to check this)", cron)
	}
	t.Logf("flags evaluated (argus-go-contract-flag = %v)", flag)

	api, token := os.Getenv("ARGUS_CONTRACT_API"), os.Getenv("ARGUS_CONTRACT_TOKEN")
	if api == "" || token == "" {
		return
	}
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		req, _ := http.NewRequest("GET", api+"/v1/issues?q="+url.QueryEscape(marker)+"&limit=5", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := http.DefaultClient.Do(req)
		if err != nil || res.StatusCode != 200 {
			if res != nil {
				res.Body.Close()
			}
			continue
		}
		var out struct {
			Issues []struct {
				ID    any    `json:"id"`
				Title string `json:"title"`
			} `json:"issues"`
		}
		_ = json.NewDecoder(res.Body).Decode(&out)
		res.Body.Close()
		for _, is := range out.Issues {
			if strings.Contains(is.Title, marker) {
				t.Logf("ok issue %v grouped from the Argus SDK", is.ID)
				return
			}
		}
	}
	t.Errorf("the error %q never showed up as an issue", marker)
}
