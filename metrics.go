package argus

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Meter records custom metrics, added up in the process and sent every 10
// seconds. Use the package's Metrics value:
//
//	argus.Metrics.Count(ctx, "orders.placed", 1, "method", "card")
//	argus.Metrics.Gauge(ctx, "queue.depth", float64(len(jobs)))
//	argus.Metrics.Histogram(ctx, "checkout.duration", ms, argus.Unit("ms"))
//
// args are slog-style attributes, plus an optional Unit. Keep attribute
// values few: every distinct set is its own series.
type Meter struct{}

// Metrics is the package's Meter.
var Metrics Meter

type metricKind int

const (
	metricSum metricKind = iota + 1
	metricGauge
	metricHistogram
)

// histogramBounds are the explicit bucket bounds every histogram uses.
var histogramBounds = []float64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

const maxSeries = 2000

type series struct {
	kind  metricKind
	name  string
	unit  Unit
	attrs []attr

	value   float64
	count   uint64
	sum     float64
	min     float64
	max     float64
	buckets []uint64
}

type metricStore struct {
	mu     sync.Mutex
	series map[string]*series
	start  time.Time
}

// Count adds value to a counter. Counters are sent as deltas.
func (Meter) Count(ctx context.Context, name string, value float64, args ...any) {
	record(metricSum, name, value, args)
}

// Gauge sets a gauge to its latest value.
func (Meter) Gauge(ctx context.Context, name string, value float64, args ...any) {
	record(metricGauge, name, value, args)
}

// Histogram records one value (a duration, a size) into a histogram.
func (Meter) Histogram(ctx context.Context, name string, value float64, args ...any) {
	record(metricHistogram, name, value, args)
}

func record(kind metricKind, name string, value float64, args []any) {
	c := get()
	if c == nil || name == "" || math.IsNaN(value) || math.IsInf(value, 0) {
		return
	}
	defer c.guard()
	attrs, unit, _ := collectAttrs(args)
	attrs = scrubAttrs(attrs)
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].key < attrs[j].key })
	var key strings.Builder
	key.WriteString(name)
	key.WriteByte(0)
	key.WriteByte(byte('0' + kind))
	key.WriteString(string(unit))
	for _, a := range attrs {
		key.WriteByte(0)
		key.WriteString(a.key)
		key.WriteByte('=')
		key.WriteString(anyString(a.val))
	}
	m := &c.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.series == nil {
		m.series = map[string]*series{}
	}
	s := m.series[key.String()]
	if s == nil {
		if len(m.series) >= maxSeries {
			c.debugf("metrics: over %d series in one interval; %q dropped", maxSeries, name)
			return
		}
		s = &series{kind: kind, name: name, unit: unit, attrs: attrs, min: math.Inf(1), max: math.Inf(-1)}
		if kind == metricHistogram {
			s.buckets = make([]uint64, len(histogramBounds)+1)
		}
		m.series[key.String()] = s
	}
	switch kind {
	case metricSum:
		s.value += value
	case metricGauge:
		s.value = value
	case metricHistogram:
		s.count++
		s.sum += value
		s.min = math.Min(s.min, value)
		s.max = math.Max(s.max, value)
		i := sort.SearchFloat64s(histogramBounds, value)
		s.buckets[i]++
	}
}

func anyString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

func (c *client) flushMetrics() {
	m := &c.metrics
	m.mu.Lock()
	if len(m.series) == 0 {
		m.mu.Unlock()
		return
	}
	all := make([]*series, 0, len(m.series))
	for _, s := range m.series {
		all = append(all, s)
	}
	start, end := m.start, time.Now()
	m.series = nil
	m.start = end
	m.mu.Unlock()
	splitEncode(len(all), maxBodyBytes,
		func(lo, hi int) []byte { return metricsRequest(c.resource, all[lo:hi], start, end) },
		func(b []byte) { c.sendOTLP(laneMetrics, "/v1/metrics", b) })
}
