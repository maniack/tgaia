package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/queue"
)

func sse(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	for _, chunk := range chunks {
		_, _ = w.Write([]byte("data: " + chunk + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func TestStreamingAssemblesContentAndToolCalls(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&request)
		sse(w,
			`{"choices":[{"delta":{"role":"assistant","reasoning_content":"think "},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"Hel"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"content":"lo","reasoning_content":"more"},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call1","type":"function","function":{"name":"echo","arguments":"{\"te"}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"xt\":\"hi\"}"}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		)
	}))
	defer server.Close()
	client, err := New(server.URL, "m", config.NewSecret(""))
	if err != nil {
		t.Fatal(err)
	}
	message, err := client.CompleteTools(context.Background(), []Message{{Role: "user", Content: "hi"}}, []Tool{{Type: "function", Function: ToolFunction{Name: "echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	if request["stream"] != true {
		t.Fatal("stream flag missing")
	}
	if message.Content != "Hello" || message.ReasoningContent != "think more" {
		t.Fatalf("content not assembled: %+v", message)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call1" || message.ToolCalls[0].Function.Name != "echo" || message.ToolCalls[0].Function.Arguments != `{"text":"hi"}` {
		t.Fatalf("tool call not assembled: %+v", message.ToolCalls)
	}
}

func TestStreamingTruncationIsTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sse(w, `{"choices":[{"delta":{"role":"assistant","content":"{\"reply\":\"partial"},"finish_reason":null}]}`, `{"choices":[{"delta":{},"finish_reason":"length"}]}`)
	}))
	defer server.Close()
	client, _ := New(server.URL, "m", config.NewSecret(""))
	_, err := client.CompleteJSON(context.Background(), []Message{{Role: "user", Content: "hi"}}, json.RawMessage(`{"type":"object"}`))
	class, terminal := queue.Classify(err)
	if err == nil || class != ClassTruncated || !terminal {
		t.Fatalf("truncation must be a terminal %s, got %v (%s)", ClassTruncated, err, class)
	}
}

func TestStreamingIdleTimeoutIsTerminalAndStallDoesNotCountAsTotal(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// A slow but live stream: 12 chunks spaced 60 ms apart exceed a 400 ms
		// per-request timeout and must still succeed under an idle watchdog.
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"role":"assistant","content":"a"}}]}` + "\n\n"))
		flusher.Flush()
		for i := 0; i < 12; i++ {
			time.Sleep(60 * time.Millisecond)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n"))
			flusher.Flush()
		}
		if r.Header.Get("X-Stall") != "" {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	defer close(release)
	cfg := config.App{Limits: map[string]string{"llm_timeout": "5s", "llm_first_token_timeout": "1s", "llm_idle_timeout": "400ms"}}
	client, err := NewWithLimits(server.URL, "m", config.NewSecret(""), cfg)
	if err != nil {
		t.Fatal(err)
	}
	text, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil || text != strings.Repeat("a", 13) {
		t.Fatalf("live slow stream must succeed, got %q %v", text, err)
	}
	stalled := *client
	stalled.http = &http.Client{Transport: headerTransport{"X-Stall", "1"}}
	start := time.Now()
	_, err = stalled.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	class, terminal := queue.Classify(err)
	if err == nil || class != ClassTimeout || !terminal {
		t.Fatalf("stalled stream must be a terminal %s, got %v (%s)", ClassTimeout, err, class)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("idle watchdog did not fire before the total ceiling")
	}
}

type headerTransport struct{ key, value string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set(h.key, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

func TestHTTPStatusClasses(t *testing.T) {
	for status, want := range map[int]struct {
		class    string
		terminal bool
	}{429: {ClassRateLimit, false}, 502: {ClassUpstream, false}, 400: {ClassRejected, true}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret", status) }))
		client, _ := New(server.URL, "m", config.NewSecret(""))
		_, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
		server.Close()
		class, terminal := queue.Classify(err)
		if err == nil || class != want.class || terminal != want.terminal || strings.Contains(err.Error(), "secret") {
			t.Fatalf("status %d: got %v (%s, terminal=%v)", status, err, class, terminal)
		}
	}
}

func TestNonStreamingGatewayFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"plain"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "m", config.NewSecret(""))
	text, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if err != nil || text != "plain" {
		t.Fatalf("plain JSON body must still parse, got %q %v", text, err)
	}
}

func TestCallerCancellationIsNotClassifiedAsModelTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body) // Let the server notice the client going away.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()
	client, _ := New(server.URL, "m", config.NewSecret(""))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.Complete(ctx, []Message{{Role: "user", Content: "hi"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller deadline must surface as context error, got %v", err)
	}
}

// A 4xx carries the status and provider error identifiers as a safe detail;
// the free-text message never leaves the client.
func TestRejectionDetailOmitsMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"prompt secret too long","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "m", config.NewSecret(""))
	_, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}})
	if got := queue.Detail(err); got != "400 invalid_request_error/context_length_exceeded" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("detail = %q err = %v", got, err)
	}
}
