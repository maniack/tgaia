package telemetry

import (
	"context"
	"errors"
	"github.com/maniack/tgaia/internal/queue"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJobMetricsAndTrace(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	p := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(p)
	defer otel.SetTracerProvider(old)
	defer p.Shutdown(context.Background())
	m := New()
	err := m.Handle(context.Background(), queue.Job{Kind: "respond"}, func(context.Context, queue.Job) error { return errors.New("secret failure text") })
	if err == nil {
		t.Fatal("lost error")
	}
	w := httptest.NewRecorder()
	m.Metrics(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `tgaia_job_errors_total{kind="respond"} 1`) {
		t.Fatal(w.Body.String())
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "job.respond" || spans[0].Status.Description != "handler failed" {
		t.Fatal(spans)
	}
}

func TestRunMetricsRecorded(t *testing.T) {
	m := New()
	err := m.Handle(context.Background(), queue.Job{Kind: "respond"}, func(ctx context.Context, _ queue.Job) error {
		jm := JobMetricsFromContext(ctx)
		if jm == nil {
			t.Fatal("JobMetrics not in context")
		}
		jm.Rounds = 3
		jm.DeniedCalls = 1
		jm.Terminal = "reply"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	m.Metrics(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		`tgaia_job_rounds_total{kind="respond"} 3`,
		`tgaia_job_denied_calls_total{kind="respond"} 1`,
		`tgaia_job_terminal_total{kind="respond",terminal="reply"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in:\n%s", want, body)
		}
	}
}
