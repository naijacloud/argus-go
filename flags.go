package argus

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// FlagClient evaluates Argus feature flags for a subject, usually a user id.
// Answers are cached for 30 seconds per subject. When Argus can't be reached
// or a flag doesn't exist, you get the default. Use the package's Flags value.
type FlagClient struct{}

// Flags is the package's FlagClient.
var Flags FlagClient

// FlagOption adjusts one evaluation.
type FlagOption func(*flagConfig)

type flagConfig struct {
	def   bool
	attrs map[string]string
}

// FlagDefault is what IsOn returns when the flag is unknown or Argus can't be
// reached. Without it, false.
func FlagDefault(on bool) FlagOption { return func(c *flagConfig) { c.def = on } }

// FlagAttributes are the subject's attributes that targeting rules match on.
func FlagAttributes(attrs map[string]string) FlagOption {
	return func(c *flagConfig) { c.attrs = attrs }
}

const flagTTL = 30 * time.Second

// IsOn reports whether a boolean flag is on for subject.
//
//	if argus.Flags.IsOn(ctx, "new-checkout", userID) { … }
func (f FlagClient) IsOn(ctx context.Context, key, subject string, opts ...FlagOption) bool {
	cfg := flagOptions(opts)
	def := "off"
	if cfg.def {
		def = "on"
	}
	v := f.Variant(ctx, key, subject, def, opts...)
	return v == "on" || v == "true"
}

// Variant returns a flag's variant for subject, or fallback.
func (FlagClient) Variant(ctx context.Context, key, subject, fallback string, opts ...FlagOption) string {
	if v, ok := evaluate(ctx, subject, flagOptions(opts).attrs)[key]; ok {
		return v
	}
	return fallback
}

// All returns every flag's variant for subject. It is empty when Argus can't
// be reached.
func (FlagClient) All(ctx context.Context, subject string, opts ...FlagOption) map[string]string {
	out := map[string]string{}
	for k, v := range evaluate(ctx, subject, flagOptions(opts).attrs) {
		out[k] = v
	}
	return out
}

func flagOptions(opts []FlagOption) flagConfig {
	var c flagConfig
	for _, o := range opts {
		if o != nil {
			o(&c)
		}
	}
	return c
}

type flagCache struct {
	mu      sync.Mutex
	entries map[string]flagEntry
}

type flagEntry struct {
	at    time.Time
	flags map[string]string
}

func evaluate(ctx context.Context, subject string, attrs map[string]string) map[string]string {
	c := get()
	if c == nil || subject == "" {
		return nil
	}
	defer c.guard()
	return c.evaluateFlags(orBackground(ctx), subject, attrs)
}

func (c *client) evaluateFlags(ctx context.Context, subject string, attrs map[string]string) map[string]string {
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var ck strings.Builder
	ck.WriteString(subject)
	for _, k := range keys {
		ck.WriteString("\x00" + k + "=" + attrs[k])
	}
	cacheKey := ck.String()

	c.flags.mu.Lock()
	hit, ok := c.flags.entries[cacheKey]
	c.flags.mu.Unlock()
	if ok && time.Since(hit.at) < flagTTL {
		return hit.flags
	}
	if !c.tr.available(laneFlags) {
		return hit.flags
	}
	if attrs == nil {
		attrs = map[string]string{}
	}
	body, _ := json.Marshal(map[string]any{"subject": subject, "environment": c.opts.Environment, "attributes": attrs})
	ctx, cancel := context.WithTimeout(internalContext(ctx), c.opts.FlagTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.dsn.url("/v1/flags/evaluate"), bytes.NewReader(body))
	if err != nil {
		return hit.flags
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.dsn.publicKey)
	req.Header.Set("User-Agent", sdkName+"/"+Version)
	res, err := c.httpc.Do(req)
	if err != nil {
		c.debugf("flags: %v; using the default", err)
		return hit.flags
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		c.tr.kill(c.tr.lane(laneFlags), res.StatusCode)
		return hit.flags
	case res.StatusCode == http.StatusTooManyRequests:
		c.tr.block(laneFlags, res.Header)
		return hit.flags
	case res.StatusCode < 200 || res.StatusCode >= 300:
		c.debugf("flags: %d; using the default", res.StatusCode)
		return hit.flags
	}
	var out struct {
		Flags map[string]string `json:"flags"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&out); err != nil {
		c.debugf("flags: unreadable answer (%v); using the default", err)
		return hit.flags
	}
	if out.Flags == nil {
		out.Flags = map[string]string{}
	}
	c.flags.mu.Lock()
	if c.flags.entries == nil || len(c.flags.entries) >= 1000 {
		c.flags.entries = map[string]flagEntry{}
	}
	c.flags.entries[cacheKey] = flagEntry{at: time.Now(), flags: out.Flags}
	c.flags.mu.Unlock()
	return out.Flags
}
