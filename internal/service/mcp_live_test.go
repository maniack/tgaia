package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/maniack/tgaia/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestLiveModelMCP(t *testing.T) {
	endpoint := os.Getenv("TEST_LM_STUDIO_URL")
	if endpoint == "" {
		t.Skip("set TEST_LM_STUDIO_URL to test live model and MCP loop")
	}
	e, _, _, scope := engineFixture(t)
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "verification", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "lookup_test_code", Description: "Return the current verification code. Use this when asked to retrieve the test verification code."}, func(ctx context.Context, req *mcp.CallToolRequest, input struct{}) (*mcp.CallToolResult, any, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Verification code: MCP-74029"}}}, nil, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer httpServer.Close()
	model, err := llm.New(endpoint, "google/gemma-4-26b-a4b-qat", config.NewSecret(""))
	if err != nil {
		t.Fatal(err)
	}
	e.Model = model
	e.Tools = &mcpclient.Provider{AgentID: "a", Bots: map[int64]string{1: "one"}, Servers: []config.MCPServer{{Name: "verify", Enabled: true, Transport: "http", URL: httpServer.URL, Bots: []string{"one"}, Tools: []string{"lookup_test_code"}}}}
	accept(t, e, scope, 1, "Вызови инструмент lookup_test_code и сообщи точный код проверки из его результата. Не придумывай код.", true)
	job := take(t, e, "respond")
	ctx, cancel := context.WithTimeout(context.Background(), 110*time.Second)
	defer cancel()
	if err = e.Respond(ctx, *job); err != nil {
		t.Fatal(err)
	}
	history, err := e.DB.Agent().Context(ctx, scope, 0, 30)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(history.Messages) < 2 || !strings.Contains(history.Messages[len(history.Messages)-1].Text, "MCP-74029") {
		t.Fatal("model did not use actual MCP tool result")
	}
	t.Log("Live LM Studio called the MCP tool and used its result in the saved response")
}
