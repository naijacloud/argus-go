package argus

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"time"
)

// attr is one attribute; val is a string, int64, float64 or bool.
type attr struct {
	key string
	val any
}

// Unit, passed among a metric's arguments, sets the metric's unit:
//
//	argus.Metrics.Histogram(ctx, "checkout.duration", ms, argus.Unit("ms"))
type Unit string

// collectAttrs reads slog-style arguments: alternating keys and values,
// slog.Attr values, and the Unit and SpanKind markers.
func collectAttrs(args []any) (attrs []attr, unit Unit, kind SpanKind) {
	if len(args) == 0 {
		return nil, "", 0
	}
	rest := make([]any, 0, len(args))
	for _, a := range args {
		switch t := a.(type) {
		case Unit:
			unit = t
		case SpanKind:
			kind = t
		default:
			rest = append(rest, a)
		}
	}
	if len(rest) == 0 {
		return nil, unit, kind
	}
	var r slog.Record
	r.Add(rest...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = flattenSlog(attrs, "", a)
		return true
	})
	return attrs, unit, kind
}

func flattenSlog(out []attr, prefix string, a slog.Attr) []attr {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p = prefix + a.Key + "."
		}
		for _, g := range v.Group() {
			out = flattenSlog(out, p, g)
		}
		return out
	}
	if a.Key == "" {
		return out
	}
	return append(out, attr{prefix + a.Key, slogValue(v)})
}

func slogValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindString:
		return v.String()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		if u := v.Uint64(); u <= math.MaxInt64 {
			return int64(u)
		}
		return float64(v.Uint64())
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindBool:
		return v.Bool()
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	default:
		return anyValue(v.Any())
	}
}

func anyValue(x any) any {
	switch t := x.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return t
	case int:
		return int64(t)
	case int64:
		return t
	case int32:
		return int64(t)
	case float64:
		return t
	case float32:
		return float64(t)
	case error:
		return t.Error()
	case fmt.Stringer:
		return t.String()
	case []byte:
		return string(t)
	}
	if b, err := json.Marshal(x); err == nil {
		return string(b)
	}
	return fmt.Sprintf("%v", x)
}

// OTLP/HTTP JSON (spec §5). Only what Argus reads.

type anyVal struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

type kv struct {
	Key   string `json:"key"`
	Value anyVal `json:"value"`
}

func attrsToKV(attrs []attr) []kv {
	out := make([]kv, 0, len(attrs))
	for _, a := range attrs {
		var v anyVal
		switch t := a.val.(type) {
		case bool:
			v.BoolValue = &t
		case int64:
			s := strconv.FormatInt(t, 10)
			v.IntValue = &s
		case float64:
			if math.IsNaN(t) || math.IsInf(t, 0) {
				s := strconv.FormatFloat(t, 'g', -1, 64)
				v.StringValue = &s
			} else {
				v.DoubleValue = &t
			}
		case string:
			if t == "" && a.key == "service.version" {
				continue
			}
			v.StringValue = &t
		default:
			s := fmt.Sprint(t)
			v.StringValue = &s
		}
		out = append(out, kv{Key: a.key, Value: v})
	}
	return out
}

func nanos(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }

var otlpScope = map[string]string{"name": "argus.go", "version": Version}

// ---- logs ----

type logRecord struct {
	time     time.Time
	severity int
	text     string
	body     string
	attrs    []attr
	traceID  string
	spanID   string
}

type otlpLog struct {
	TimeUnixNano         string `json:"timeUnixNano"`
	ObservedTimeUnixNano string `json:"observedTimeUnixNano"`
	SeverityNumber       int    `json:"severityNumber"`
	SeverityText         string `json:"severityText"`
	Body                 anyVal `json:"body"`
	Attributes           []kv   `json:"attributes"`
	TraceID              string `json:"traceId,omitempty"`
	SpanID               string `json:"spanId,omitempty"`
}

func logsRequest(resource []kv, recs []logRecord) []byte {
	out := make([]otlpLog, len(recs))
	for i, r := range recs {
		body := r.body
		out[i] = otlpLog{
			TimeUnixNano:         nanos(r.time),
			ObservedTimeUnixNano: nanos(r.time),
			SeverityNumber:       r.severity,
			SeverityText:         r.text,
			Body:                 anyVal{StringValue: &body},
			Attributes:           attrsToKV(r.attrs),
			TraceID:              r.traceID,
			SpanID:               r.spanID,
		}
	}
	b, _ := json.Marshal(map[string]any{
		"resourceLogs": []any{map[string]any{
			"resource":  map[string]any{"attributes": resource},
			"scopeLogs": []any{map[string]any{"scope": otlpScope, "logRecords": out}},
		}},
	})
	return b
}

// ---- traces ----

type spanData struct {
	traceID, spanID, parentSpanID string
	name                          string
	kind                          SpanKind
	start, end                    time.Time
	errMsg                        string
	attrs                         []attr
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []kv           `json:"attributes"`
	Status            map[string]any `json:"status"`
}

func tracesRequest(resource []kv, spans []spanData) []byte {
	out := make([]otlpSpan, len(spans))
	for i, s := range spans {
		status := map[string]any{"code": 1}
		if s.errMsg != "" {
			status = map[string]any{"code": 2, "message": s.errMsg}
		}
		out[i] = otlpSpan{
			TraceID:           s.traceID,
			SpanID:            s.spanID,
			ParentSpanID:      s.parentSpanID,
			Name:              s.name,
			Kind:              int(s.kind),
			StartTimeUnixNano: nanos(s.start),
			EndTimeUnixNano:   nanos(s.end),
			Attributes:        attrsToKV(s.attrs),
			Status:            status,
		}
	}
	b, _ := json.Marshal(map[string]any{
		"resourceSpans": []any{map[string]any{
			"resource":   map[string]any{"attributes": resource},
			"scopeSpans": []any{map[string]any{"scope": otlpScope, "spans": out}},
		}},
	})
	return b
}

// ---- metrics ----

// The delta temporality value in OTLP's AggregationTemporality enum.
const temporalityDelta = 1

func metricsRequest(resource []kv, series []*series, start, end time.Time) []byte {
	metrics := make([]map[string]any, 0, len(series))
	for _, s := range series {
		m := map[string]any{"name": s.name}
		if s.unit != "" {
			m["unit"] = string(s.unit)
		}
		attrs := attrsToKV(s.attrs)
		switch s.kind {
		case metricSum:
			m["sum"] = map[string]any{
				"aggregationTemporality": temporalityDelta,
				"isMonotonic":            true,
				"dataPoints": []any{map[string]any{
					"attributes": attrs, "startTimeUnixNano": nanos(start), "timeUnixNano": nanos(end), "asDouble": s.value,
				}},
			}
		case metricGauge:
			m["gauge"] = map[string]any{
				"dataPoints": []any{map[string]any{"attributes": attrs, "timeUnixNano": nanos(end), "asDouble": s.value}},
			}
		case metricHistogram:
			buckets := make([]string, len(s.buckets))
			for i, n := range s.buckets {
				buckets[i] = strconv.FormatUint(n, 10)
			}
			m["histogram"] = map[string]any{
				"aggregationTemporality": temporalityDelta,
				"dataPoints": []any{map[string]any{
					"attributes":        attrs,
					"startTimeUnixNano": nanos(start),
					"timeUnixNano":      nanos(end),
					"count":             strconv.FormatUint(s.count, 10),
					"sum":               s.sum,
					"min":               s.min,
					"max":               s.max,
					"explicitBounds":    histogramBounds,
					"bucketCounts":      buckets,
				}},
			}
		}
		metrics = append(metrics, m)
	}
	b, _ := json.Marshal(map[string]any{
		"resourceMetrics": []any{map[string]any{
			"resource":     map[string]any{"attributes": resource},
			"scopeMetrics": []any{map[string]any{"scope": otlpScope, "metrics": metrics}},
		}},
	})
	return b
}

// splitEncode encodes n records with enc, halving the batch until each body
// is under max (spec §3: SDKs split batches below the size limits). A single
// record still too big is dropped.
func splitEncode(n int, max int, enc func(lo, hi int) []byte, emit func([]byte)) {
	var rec func(lo, hi int)
	rec = func(lo, hi int) {
		if lo >= hi {
			return
		}
		b := enc(lo, hi)
		if len(b) <= max {
			emit(b)
			return
		}
		if hi-lo == 1 {
			return
		}
		mid := (lo + hi) / 2
		rec(lo, mid)
		rec(mid, hi)
	}
	rec(0, n)
}
