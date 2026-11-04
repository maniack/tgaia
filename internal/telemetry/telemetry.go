// Package telemetry exposes bounded, content-free metrics and OTLP traces.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

type measurement struct{ count, failures, nanos, inflight atomic.Int64 }
type Monitor struct {
	WorkerRunning atomic.Bool
	mu            sync.Mutex
	endpoint      string
	provider      *sdktrace.TracerProvider
	jobs          map[string]*measurement
}

func New() *Monitor {
	return &Monitor{jobs: map[string]*measurement{"respond": {}, "compact": {}, "message_action": {}}}
}
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
	measurement := m.jobs[j.Kind]
	if measurement == nil {
		return h(ctx, j)
	}
	start := time.Now()
	measurement.inflight.Add(1)
	ctx, span := otel.Tracer("tgaia/worker").Start(ctx, "job."+j.Kind)
	defer span.End()
	defer func() {
		measurement.inflight.Add(-1)
		measurement.count.Add(1)
		measurement.nanos.Add(time.Since(start).Nanoseconds())
	}()
	err := h(ctx, j)
	if err != nil {
		measurement.failures.Add(1)
		span.SetStatus(codes.Error, "handler failed")
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
	fmt.Fprintln(w, "# TYPE tgaia_jobs_total counter\n# TYPE tgaia_job_errors_total counter\n# TYPE tgaia_job_duration_seconds_sum counter\n# TYPE tgaia_jobs_inflight gauge")
	for _, kind := range []string{"respond", "compact", "message_action"} {
		v := m.jobs[kind]
		fmt.Fprintf(w, "tgaia_jobs_total{kind=%q} %d\ntgaia_job_errors_total{kind=%q} %d\ntgaia_job_duration_seconds_sum{kind=%q} %f\ntgaia_jobs_inflight{kind=%q} %d\n", kind, v.count.Load(), kind, v.failures.Load(), kind, float64(v.nanos.Load())/1e9, kind, v.inflight.Load())
	}
}
