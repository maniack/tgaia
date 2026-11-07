package mcpclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testServer(calls *atomic.Int32) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "Echo test text"}, func(ctx context.Context, r *mcp.CallToolRequest, in struct {
		Text string `json:"text"`
	}) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: in.Text}}}, nil, nil
	})
	return s
}
func TestRemoteTransportsAndPolicy(t *testing.T) {
	for _, transport := range []string{"http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			var calls atomic.Int32
			s := testServer(&calls)
			var handler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
			if transport == "sse" {
				handler = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return s }, nil)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("missing bearer credential")
					w.WriteHeader(401)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			var revoked atomic.Bool
			p := Provider{Permit: func(chat.Scope, string, string) bool { return !revoked.Load() }, AgentID: "a", Bots: map[int64]string{1: "one", 2: "two"}, Servers: []config.MCPServer{{Name: "test", Enabled: true, Transport: transport, URL: server.URL, Token: config.NewSecret("test-secret"), Bots: []string{"one"}, Tools: []string{"echo"}}}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			session, err := p.Open(ctx, chat.Scope{AgentID: "a", BotID: 1, ChatID: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			specs := session.Tools()
			if len(specs) != 1 {
				t.Fatal("discovery failed")
			}
			name := specs[0].Function.Name
			if _, err := session.Call(ctx, name, `{"text":"hello"}`); err != nil {
				t.Fatal(err)
			}
			if _, err := session.Call(ctx, name, `{"text":42}`); err == nil {
				t.Fatal("invalid args accepted")
			}
			if _, err := session.Call(ctx, "unlisted", `{}`); err == nil {
				t.Fatal("unlisted tool accepted")
			}
			revoked.Store(true)
			if _, err := session.Call(ctx, name, `{"text":"revoked"}`); err == nil {
				t.Fatal("revoked tool executed on existing session")
			}
			other, err := p.Open(ctx, chat.Scope{AgentID: "a", BotID: 2, ChatID: 1})
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			if len(other.Tools()) != 0 || calls.Load() != 1 {
				t.Fatal("bot allowlist or validation bypassed")
			}
		})
	}
}
func TestStdioHelper(t *testing.T) {
	if os.Getenv("MCP_TEST_HELPER") != "1" {
		return
	}
	var calls atomic.Int32
	if os.Getenv("TGAIA_SESSION_KEY") != "" {
		os.Exit(2)
	}
	_ = testServer(&calls).Run(context.Background(), &mcp.StdioTransport{})
	os.Exit(0)
}
func TestStdioLifecycle(t *testing.T) {
	t.Setenv("TGAIA_SESSION_KEY", "must-not-inherit")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := Provider{AgentID: "a", Bots: map[int64]string{1: "one"}, Servers: []config.MCPServer{{Name: "stdio", Enabled: true, Transport: "stdio", Command: executable, Args: []string{"-test.run=^TestStdioHelper$"}, Env: config.NewSecret(`{"MCP_TEST_HELPER":"1"}`), Bots: []string{"one"}, Tools: []string{"echo"}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := p.Open(ctx, chat.Scope{AgentID: "a", BotID: 1, ChatID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Call(ctx, session.Tools()[0].Function.Name, `{"text":"stdio"}`); err != nil {
		t.Fatal(err)
	}
	session.Close()
}

// An oversized result keeps its beginning and says it is truncated; the old
// behaviour (dropping it entirely) left the model with nothing but a refusal.
func TestOversizedResultIsTruncatedNotDropped(t *testing.T) {
	body := strings.Repeat("Полезный текст README. ", 2000)
	out := truncateResult(false, []string{body}, 4000)
	if len(out) > 4000 {
		t.Fatalf("truncated result is %d bytes", len(out))
	}
	var r struct {
		IsError   bool     `json:"is_error"`
		Truncated bool     `json:"truncated"`
		Content   []string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err, out)
	}
	if r.IsError || !r.Truncated || len(r.Content) != 2 || !strings.HasPrefix(r.Content[0], "Полезный текст README.") || !strings.Contains(r.Content[1], "truncated") {
		t.Fatalf("unexpected shape: %+v", r)
	}
	if !utf8.ValidString(r.Content[0]) {
		t.Fatal("cut inside a UTF-8 sequence")
	}
}

func TestToolNameReadableWithHashOnlyWhenNeeded(t *testing.T) {
	none := func(string) bool { return false }
	if got := toolName("k8s", "list_in_namespace", none); got != "k8s__list_in_namespace" {
		t.Fatalf("plain name = %q", got)
	}
	if got := toolName("web", "fetch.url/v2", none); got != "web__fetch_url_v2" {
		t.Fatalf("sanitized name = %q", got)
	}
	taken := func(n string) bool { return n == "web__fetch_url" }
	a, b := toolName("web", "fetch.url", taken), toolName("web", "fetch/url", taken)
	if a == "web__fetch_url" || a == b || !strings.HasPrefix(a, "web__fetch_url_") {
		t.Fatalf("collision names = %q %q", a, b)
	}
	long := toolName("server", strings.Repeat("x", 100), none)
	if len(long) != maxToolName || long != toolName("server", strings.Repeat("x", 100), none) {
		t.Fatalf("long name = %q (%d)", long, len(long))
	}
}
