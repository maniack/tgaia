// Package telemetry exposes bounded, content-free metrics and OTLP traces.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maniack/tgaia/internal/queue"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// JobMetrics is injected into the handler context by Monitor.Handle so that
// Engine can record per-job run statistics without a direct dependency on Monitor.
type JobMetrics struct {
	Rounds      int64
	DeniedCalls int64
	Terminal    string
}

type jobMetricsKey struct{}

// WithJobMetrics returns a child context carrying a *JobMetrics and the pointer.
func WithJobMetrics(ctx context.Context) (context.Context, *JobMetrics) {
	m := &JobMetrics{}
	return context.WithValue(ctx, jobMetricsKey{}, m), m
}

// JobMetricsFromContext retrieves the *JobMetrics injected by Monitor.Handle.
// Returns nil when called outside a monitored handler.
func JobMetricsFromContext(ctx context.Context) *JobMetrics {
	v, _ := ctx.Value(jobMetricsKey{}).(*JobMetrics)
	return v
}

type measurement struct {
	count, failures, nanos, inflight atomic.Int64
	rounds, denied                   atomic.Int64
	mu                               sync.Mutex
	classes                          map[string]int64
	terminals                        map[string]int64
}
type Monitor struct {
	WorkerRunning atomic.Bool
	mu            sync.Mutex
	endpoint      string
	provider      *sdktrace.TracerProvider
	jobs          map[string]*measurement
}

func New() *Monitor {
	return &Monitor{jobs: map[string]*measurement{
		"respond":        {classes: map[string]int64{}, terminals: map[string]int64{}},
		"compact":        {classes: map[string]int64{}, terminals: map[string]int64{}},
		"message_action": {classes: map[string]int64{}, terminals: map[string]int64{}},
	}}
}

// failureClasses bounds the label set; classes outside it count as "other".
var failureClasses = map[string]bool{"llm_timeout": true, "llm_429": true, "llm_http_5xx": true, "llm_http_4xx": true, "llm_network": true, "llm_truncated": true, "llm_invalid": true, "llm_budget": true, "embedding_timeout": true, "embedding_failed": true, "mcp_timeout": true, "mcp_failed": true, "quota": true, "budget": true, "panic": true, "job_timeout": true, "canceled": true, "lease_lost": true, "handler_error": true}

func (m *Monitor) Configure(ctx context.Context, endpoint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.provider != nil && m.endpoint == endpoint {
		return nil
	}
	options := []sdktrace.TracerProviderOption{sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", "tgaia")))}
	if endpoint != "" {
		exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint), otlptracehttp.WithTimeout(5*time.Second))
		if err != nil {
			return errors.New("OTLP initialization failed")
		}
		options = append(options, sdktrace.WithBatcher(exporter), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))))
	} else {
		options = append(options, sdktrace.WithSampler(sdktrace.NeverSample()))
	}
	previous := m.provider
	m.provider = sdktrace.NewTracerProvider(options...)
	m.endpoint = endpoint
	otel.SetTracerProvider(m.provider)
	if previous != nil {
		// #nosec G118 -- exporter shutdown must outlive the configuration request
		// that replaced it; a bounded independent context prevents a leak.
		go func() {
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			_ = previous.Shutdown(ctx)
		}()
	}
	return nil
}
func (m *Monitor) Close() {
	m.mu.Lock()
	p := m.provider
	m.mu.Unlock()
	if p != nil {
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = p.Shutdown(ctx)
	}
}
func (m *Monitor) Handle(ctx context.Context, j queue.Job, h queue.Handler) error {
	meas := m.jobs[j.Kind]
	if meas == nil {
		return h(ctx, j)
	}
	start := time.Now()
	meas.inflight.Add(1)
	ctx, span := otel.Tracer("tgaia/worker").Start(ctx, "job."+j.Kind)
	defer span.End()
	ctx, jm := WithJobMetrics(ctx)
	err := h(ctx, j)
	meas.inflight.Add(-1)
	meas.count.Add(1)
	meas.nanos.Add(time.Since(start).Nanoseconds())
	meas.rounds.Add(jm.Rounds)
	meas.denied.Add(jm.DeniedCalls)
	if jm.Terminal != "" {
		meas.mu.Lock()
		meas.terminals[jm.Terminal]++
		meas.mu.Unlock()
	}
	if err != nil {
		meas.failures.Add(1)
		class, terminal := queue.Classify(err)
		if !failureClasses[class] {
			class = "other"
		}
		meas.mu.Lock()
		meas.classes[class]++
		meas.mu.Unlock()
		span.SetStatus(codes.Error, "handler failed")
		span.SetAttributes(attribute.String("failure.class", class), attribute.Bool("failure.terminal", terminal), attribute.String("failure.phase", queue.Phase(err)))
	}
	return err
}
func (m *Monitor) Metrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintln(w, "# TYPE tgaia_worker_running gauge")
	running := 0
	if m.WorkerRunning.Load() {
		running = 1
	}
	fmt.Fprintf(w, "tgaia_worker_running %d\n", running)
	fmt.Fprintln(w, "# TYPE tgaia_jobs_total counter\n# TYPE tgaia_job_errors_total counter\n# TYPE tgaia_job_duration_seconds_sum counter\n# TYPE tgaia_jobs_inflight gauge\n# TYPE tgaia_job_rounds_total counter\n# TYPE tgaia_job_denied_calls_total counter")
	for _, kind := range []string{"respond", "compact", "message_action"} {
		v := m.jobs[kind]
		fmt.Fprintf(w, "tgaia_jobs_total{kind=%q} %d\ntgaia_job_errors_total{kind=%q} %d\ntgaia_job_duration_seconds_sum{kind=%q} %f\ntgaia_jobs_inflight{kind=%q} %d\ntgaia_job_rounds_total{kind=%q} %d\ntgaia_job_denied_calls_total{kind=%q} %d\n",
			kind, v.count.Load(),
			kind, v.failures.Load(),
			kind, float64(v.nanos.Load())/1e9,
			kind, v.inflight.Load(),
			kind, v.rounds.Load(),
			kind, v.denied.Load(),
		)
	}
	fmt.Fprintln(w, "# TYPE tgaia_queue_failures_total counter")
	for _, kind := range []string{"respond", "compact", "message_action"} {
		v := m.jobs[kind]
		v.mu.Lock()
		classes := make([]string, 0, len(v.classes))
		for class := range v.classes {
			classes = append(classes, class)
		}
		sort.Strings(classes)
		for _, class := range classes {
			fmt.Fprintf(w, "tgaia_queue_failures_total{kind=%q,class=%q} %d\n", kind, class, v.classes[class])
		}
		terminals := make([]string, 0, len(v.terminals))
		for t := range v.terminals {
			terminals = append(terminals, t)
		}
		sort.Strings(terminals)
		termCounts := make([]int64, len(terminals))
		for i, t := range terminals {
			termCounts[i] = v.terminals[t]
		}
		v.mu.Unlock()
		if len(terminals) > 0 {
			if kind == "respond" {
				fmt.Fprintln(w, "# TYPE tgaia_job_terminal_total counter")
			}
			for i, t := range terminals {
				fmt.Fprintf(w, "tgaia_job_terminal_total{kind=%q,terminal=%q} %d\n", kind, t, termCounts[i])
			}
		}
	}
}
