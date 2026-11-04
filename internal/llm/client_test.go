package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/config"
)

func TestOpenAICompatibleTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("wrong transport endpoint/auth")
		}
		var request struct {
			Model    string    `json:"model"`
			Messages []Message `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "Default" {
			t.Error("wrong model")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "Default", config.NewSecret("test-key"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "test"}})
	if err != nil || response != "ok" {
		t.Fatal("completion failed")
	}
}
func TestModelErrorRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret response", 500) }))
	defer server.Close()
	client, _ := New(server.URL, "Default", config.NewSecret("secret-key"))
	_, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "private prompt"}})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") {
		t.Fatal("provider error leaked data")
	}
}
func TestLiveLMStudio(t *testing.T) {
	endpoint := os.Getenv("TEST_LM_STUDIO_URL")
	if endpoint == "" {
		t.Skip("set TEST_LM_STUDIO_URL for explicit local-model integration")
	}
	client, err := New(endpoint, "google/gemma-4-26b-a4b-qat", config.NewSecret(""))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"reply":{"type":"string"},"schedule":{"type":"null"}},"required":["reply","schedule"]}`)
	reply, err := client.CompleteJSON(ctx, []Message{{Role: "system", Content: `Return only JSON with fields reply (string) and schedule (null).`}, {Role: "user", Content: "Say hello in Russian in one sentence."}}, schema)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Reply    string `json:"reply"`
		Schedule any    `json:"schedule"`
	}
	if json.Unmarshal([]byte(reply), &data) != nil || data.Reply == "" || data.Schedule != nil {
		t.Fatal("local model did not return expected JSON")
	}
	t.Log("Configured LM Studio model returned a valid structured response")
}

func TestNativeToolCalling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["tools"] == nil || body["response_format"] != nil {
			t.Error("invalid tool request")
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"echo","arguments":"{\"text\":\"hello\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "Default", config.NewSecret(""))
	response, err := client.CompleteTools(context.Background(), []Message{{Role: "user", Content: "echo hello"}}, []Tool{{Type: "function", Function: ToolFunction{Name: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil || len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "call1" {
		t.Fatal("native tool response not decoded", err)
	}
}

func TestNativeToolCallingWithReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning_content":"Thinking step: need to search web","tool_calls":[{"id":"call_search","type":"function","function":{"name":"search","arguments":"{\"query\":\"golang\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer server.Close()
	client, _ := New(server.URL, "Default", config.NewSecret(""))
	response, err := client.CompleteTools(context.Background(), []Message{{Role: "user", Content: "search golang"}}, []Tool{{Type: "function", Function: ToolFunction{Name: "search", Parameters: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil {
		t.Fatal("tool call failed:", err)
	}
	if response.ReasoningContent != "Thinking step: need to search web" {
		t.Fatalf("reasoning_content not decoded, got %q", response.ReasoningContent)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Function.Name != "search" {
		t.Fatalf("expected tool call 'search', got %+v", response.ToolCalls)
	}
}
