// Package observability wires logging, metrics and request correlation.
//
// The goal is that one identifier ties a spoken command to every log line, every
// metric sample and every audit row it produced. Without that, "it was slow
// once" is unanswerable.
package observability

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/neeldas0032/NeelKIT-AI-voice-assistant/internal/observability/metrics"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxSessionID
)

// WithRequestID attaches a request id to the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// RequestID reads the request id, or "" when absent.
func RequestID(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// WithSessionID attaches a realtime session id.
func WithSessionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxSessionID, id)
}

// SessionID reads the session id, or "" when absent.
func SessionID(ctx context.Context) string {
	v, _ := ctx.Value(ctxSessionID).(string)
	return v
}

// Logger returns a logger carrying whatever correlation ids the context holds.
// Handlers use this instead of slog.Default so every line is joinable.
func Logger(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if id := RequestID(ctx); id != "" {
		l = l.With("request_id", id)
	}
	if id := SessionID(ctx); id != "" {
		l = l.With("session_id", id)
	}
	return l
}

// SetupLogging installs the global logger.
func SetupLogging(level, format string) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}

	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}

// Metrics holds every instrument the server exports.
//
// They are fields on a struct rather than package-level variables so tests can
// build an isolated set and a second registry cannot collide with the first.
type Metrics struct {
	Registry *metrics.Registry

	HTTPRequests *metrics.CounterVec
	HTTPDuration *metrics.HistogramVec

	ASRDuration  *metrics.HistogramVec
	ASRQueueWait *metrics.Histogram
	ASRQueueFree *metrics.Gauge
	ASRRTF       *metrics.Histogram
	ASRShed      *metrics.CounterVec

	StreamPartialLatency *metrics.Histogram
	StreamFinalLatency   *metrics.Histogram
	StreamSessions       *metrics.Gauge

	IntentResolved *metrics.CounterVec
	LLMDuration    *metrics.Histogram
	LLMTokens      *metrics.CounterVec

	ToolDuration *metrics.HistogramVec
	ToolDecision *metrics.CounterVec
}

// Latency buckets. The defaults most libraries ship top out around ten
// seconds, which would collapse the entire tail of a long transcription into
// one bucket and hide exactly the cases worth investigating.
var (
	httpBuckets   = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
	asrBuckets    = []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}
	queueBuckets  = []float64{.001, .01, .05, .1, .25, .5, 1, 2.5, 5, 10}
	rtfBuckets    = []float64{.05, .1, .2, .3, .5, .75, 1, 1.5, 2, 5}
	streamBuckets = []float64{.05, .1, .2, .3, .5, .75, 1, 2, 5, 10}
	llmBuckets    = []float64{.1, .25, .5, 1, 2, 5, 10, 20, 60}
	toolBuckets   = []float64{.005, .05, .1, .5, 1, 5, 15, 30, 60, 300}
)

// NewMetrics builds the instrument set.
func NewMetrics() *Metrics {
	r := metrics.NewRegistry()
	return &Metrics{
		Registry: r,

		HTTPRequests: r.NewCounterVec("neelkit_http_requests_total",
			"HTTP requests by route, method and status.", []string{"route", "method", "status"}),
		HTTPDuration: r.NewHistogramVec("neelkit_http_request_duration_seconds",
			"HTTP request latency.", []string{"route", "method"}, httpBuckets),

		ASRDuration: r.NewHistogramVec("neelkit_asr_inference_seconds",
			"Model inference time by stage and priority.", []string{"stage", "priority"}, asrBuckets),
		ASRQueueWait: r.NewHistogram("neelkit_asr_queue_wait_seconds",
			"Time a transcription waited for a free decoder worker.", queueBuckets),
		ASRQueueFree: r.NewGauge("neelkit_asr_workers_free",
			"Decoder workers currently idle."),
		ASRRTF: r.NewHistogram("neelkit_asr_real_time_factor",
			"Inference time divided by audio duration; below 1.0 is faster than real time.", rtfBuckets),
		ASRShed: r.NewCounterVec("neelkit_asr_shed_total",
			"Transcriptions dropped or rejected under load, by priority.", []string{"priority"}),

		StreamPartialLatency: r.NewHistogram("neelkit_stream_partial_latency_seconds",
			"Time from audio arriving to a partial transcript being sent.", streamBuckets),
		StreamFinalLatency: r.NewHistogram("neelkit_stream_final_latency_seconds",
			"Time from endpoint detection to the final transcript being sent.", streamBuckets),
		StreamSessions: r.NewGauge("neelkit_stream_sessions_active",
			"Open realtime sessions."),

		IntentResolved: r.NewCounterVec("neelkit_intent_resolved_total",
			"Intents resolved, by source and name.", []string{"source", "intent"}),
		LLMDuration: r.NewHistogram("neelkit_llm_duration_seconds",
			"Language-model completion latency.", llmBuckets),
		LLMTokens: r.NewCounterVec("neelkit_llm_tokens_total",
			"Tokens consumed, by direction.", []string{"direction"}),

		ToolDuration: r.NewHistogramVec("neelkit_tool_execution_seconds",
			"Tool execution time by tool and outcome.", []string{"tool", "outcome"}, toolBuckets),
		ToolDecision: r.NewCounterVec("neelkit_tool_decisions_total",
			"Policy decisions by tool, risk and decision.", []string{"tool", "risk", "decision"}),
	}
}

// ObserveHTTP records one request.
func (m *Metrics) ObserveHTTP(route, method, status string, d time.Duration) {
	m.HTTPRequests.Inc(route, method, status)
	m.HTTPDuration.Observe(d.Seconds(), route, method)
}
