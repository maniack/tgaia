// Package mcpclient adapts the official SDK to a bounded, per-job tool session.
package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/llm"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const CallTimeout = 20 * time.Second
const MaxResult = 16000

type Provider struct {
	Config  config.App
	Permit  func(scope chat.Scope, server, tool string) bool
	AgentID string
	Bots    map[int64]string
	Servers []config.MCPServer
}
type Session interface {
	Tools() []llm.Tool
	Validate(string, string) error
	Call(context.Context, string, string) (string, error)
	Close()
}
type entry struct {
	permit  func() bool
	session *mcp.ClientSession
	name    string
	schema  *jsonschema.Resolved
}
type session struct {
	timeout   time.Duration
	maxResult int
	entries   map[string]entry
	tools     []llm.Tool
	sessions  []*mcp.ClientSession
	cleanups  []func()
}

func (s *session) Close() {
	var closing sync.WaitGroup
	for _, cs := range s.sessions {
		closing.Add(1)
		go func() { defer closing.Done(); _ = cs.Close() }()
	}
	closing.Wait()
	for _, f := range s.cleanups {
		f()
	}
}
func (s *session) Tools() []llm.Tool { return s.tools }
func allows(list []string, v string) bool {
	return slices.Contains(list, "*") || slices.Contains(list, v)
}
func (p *Provider) Open(ctx context.Context, scope chat.Scope) (Session, error) {
	s := &session{timeout: p.Config.Duration("mcp_call_timeout"), maxResult: p.Config.Limit("mcp_result_bytes"), entries: map[string]entry{}}
	if !scope.Valid() || scope.AgentID != p.AgentID || p.Bots[scope.BotID] == "" {
		return nil, errors.New("invalid MCP scope")
	}
	for _, server := range p.Servers {
		if !server.Enabled || !allows(server.Bots, p.Bots[scope.BotID]) {
			continue
		}
		transport, cleanup, err := connectTransport(ctx, server, s.timeout)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.cleanups = append(s.cleanups, cleanup)
		// Never advertise sampling, roots, elicitation or credential callbacks.
		client := mcp.NewClient(&mcp.Implementation{Name: "tgaia", Version: "poc"}, &mcp.ClientOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		connectCtx, closeConnection := context.WithCancel(ctx)
		// SSE owns a long-lived GET using the Connect context. Keep it alive
		// after initialization; only the initialization timer is stopped.
		timer := time.AfterFunc(s.timeout, closeConnection)
		cs, err := client.Connect(connectCtx, transport, nil)
		timer.Stop()
		s.cleanups = append(s.cleanups, closeConnection)
		if err != nil {
			s.Close()
			return nil, errors.New("MCP connection failed")
		}
		s.sessions = append(s.sessions, cs)
		listCtx, cancel := context.WithTimeout(ctx, s.timeout)
		count := 0
		for tool, err := range cs.Tools(listCtx, nil) {
			count++
			if err != nil || count > 256 {
				cancel()
				s.Close()
				return nil, errors.New("MCP tool discovery failed or exceeded limit")
			}
			if !allows(server.Tools, tool.Name) {
				continue
			}
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil || len(schema) > 16000 || len(tool.Description) > 4000 || len(tool.Name) > 256 {
				cancel()
				s.Close()
				return nil, errors.New("MCP tool definition exceeds limit")
			}
			// Some OpenAI-compatible providers require properties even for an
			// empty-object schema. Adding it preserves JSON Schema semantics.
			var shape map[string]json.RawMessage
			if json.Unmarshal(schema, &shape) != nil || shape == nil {
				cancel()
				s.Close()
				return nil, errors.New("invalid MCP schema object")
			}
			if _, ok := shape["properties"]; !ok {
				shape["properties"] = json.RawMessage(`{}`)
				schema, _ = json.Marshal(shape)
			}
			var input jsonschema.Schema
			if json.Unmarshal(schema, &input) != nil || input.Type != "object" {
				cancel()
				s.Close()
				return nil, errors.New("invalid MCP input schema")
			}
			resolved, err := input.Resolve(nil) // No remote schema fetches.
			if err != nil {
				cancel()
				s.Close()
				return nil, errors.New("unsupported MCP input schema")
			}
			hash := sha256.Sum256([]byte(server.Name + "\x00" + tool.Name))
			name := fmt.Sprintf("mcp_%x", hash[:16])
			s.entries[name] = entry{session: cs, name: tool.Name, schema: resolved, permit: func() bool { return p.Permit == nil || p.Permit(scope, server.Name, tool.Name) }}
			s.tools = append(s.tools, llm.Tool{Type: "function", Function: llm.ToolFunction{Name: name, Description: server.Name + " / " + tool.Name + ": " + tool.Description, Parameters: schema}})
			if len(s.tools) > 64 {
				cancel()
				s.Close()
				return nil, errors.New("too many allowed MCP tools")
			}
		}
		cancel()
	}
	sort.Slice(s.tools, func(i, j int) bool { return s.tools[i].Function.Name < s.tools[j].Function.Name })
	return s, nil
}
func (s *session) Validate(name, args string) error {
	e, ok := s.entries[name]
	if !ok || (e.permit != nil && !e.permit()) {
		return errors.New("tool is not permitted for this bot")
	}
	if len(args) > 32000 {
		return errors.New("tool arguments exceed limit")
	}
	var value map[string]any
	if json.Unmarshal([]byte(args), &value) != nil || value == nil || e.schema.Validate(value) != nil {
		return errors.New("tool arguments do not match schema")
	}
	return nil
}
func (s *session) Call(ctx context.Context, name, args string) (string, error) {
	ctx, span := otel.Tracer("tgaia").Start(ctx, "mcp.call")
	defer span.End()
	if err := s.Validate(name, args); err != nil {
		return "", err
	}
	e := s.entries[name]
	callCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var arguments map[string]any
	_ = json.Unmarshal([]byte(args), &arguments)
	result, err := e.session.CallTool(callCtx, &mcp.CallToolParams{Name: e.name, Arguments: arguments})
	if err != nil {
		return "", errors.New("MCP call failed; outcome unknown")
	}
	var content []string
	for _, block := range result.Content {
		if text, ok := block.(*mcp.TextContent); ok {
			content = append(content, text.Text)
		} else {
			content = append(content, "[Non-text tool content omitted]")
		}
	}
	data, err := json.Marshal(map[string]any{"is_error": result.IsError, "content": content, "structured_content": result.StructuredContent})
	if err != nil {
		return "", errors.New("invalid MCP result")
	}
	if len(data) > s.maxResult {
		return truncateResult(result.IsError, content, s.maxResult), nil
	}
	return string(data), nil
}

// truncateResult keeps the beginning of an oversized result instead of
// dropping it: the first kilobytes of a page or file usually carry the
// answer, and the model is told explicitly that the rest is missing.
func truncateResult(isError bool, content []string, limit int) string {
	const notice = "[truncated: the result exceeded the size limit; only its beginning is shown. Do not repeat the action.]"
	joined := strings.Join(content, "\n")
	budget := limit - len(notice) - 160 // JSON framing and escaping headroom.
	if budget < 0 {
		budget = 0
	}
	if len(joined) > budget {
		joined = joined[:budget]
		for !utf8.ValidString(joined) && len(joined) > 0 {
			joined = joined[:len(joined)-1]
		}
	}
	for {
		data, _ := json.Marshal(map[string]any{"is_error": isError, "truncated": true, "content": []string{joined, notice}})
		if len(data) <= limit || len(joined) == 0 {
			return string(data)
		}
		cut := len(joined) - (len(data) - limit) - 16
		if cut < 0 {
			cut = 0
		}
		joined = joined[:cut]
		for !utf8.ValidString(joined) && len(joined) > 0 {
			joined = joined[:len(joined)-1]
		}
	}
}

type authenticatedTransport struct {
	base   *http.Transport
	origin *url.URL
	token  config.Secret
}

func (t *authenticatedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != t.origin.Scheme || r.URL.Host != t.origin.Host {
		return nil, errors.New("MCP cross-origin request rejected")
	}
	clone := r.Clone(r.Context())
	if t.token.Reveal() != "" {
		clone.Header.Set("Authorization", "Bearer "+t.token.Reveal())
	}
	response, err := t.base.RoundTrip(clone)
	if err == nil && !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		response.Body = &limitedBody{ReadCloser: response.Body, left: 1 << 20}
	}
	return response, err
}

type limitedBody struct {
	io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errors.New("MCP response exceeds limit")
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}
func connectTransport(ctx context.Context, s config.MCPServer, timeouts ...time.Duration) (mcp.Transport, func(), error) {
	if s.Transport == "stdio" {
		// #nosec G204 -- only administrators configure this feature; validation
		// requires an absolute executable and execution never passes through a shell.
		command := exec.CommandContext(ctx, s.Command, s.Args...)
		command.Env = []string{} // Do not inherit Telegram/DB/OIDC credentials.
		var env map[string]string
		if json.Unmarshal([]byte(s.Env.Reveal()), &env) != nil {
			return nil, nil, errors.New("invalid MCP environment")
		}
		for k, v := range env {
			command.Env = append(command.Env, k+"="+v)
		}
		command.Stderr = io.Discard
		return &mcp.CommandTransport{Command: command, TerminateDuration: time.Second}, func() {}, nil
	}
	origin, err := url.Parse(s.URL)
	if err != nil {
		return nil, nil, errors.New("invalid MCP URL")
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.ResponseHeaderTimeout = CallTimeout
	if len(timeouts) > 0 {
		base.ResponseHeaderTimeout = timeouts[0]
	}
	client := &http.Client{Transport: &authenticatedTransport{base: base, origin: origin, token: s.Token}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if s.Transport == "sse" {
		return &mcp.SSEClientTransport{Endpoint: s.URL, HTTPClient: client, MaxEventSize: 1 << 20}, base.CloseIdleConnections, nil
	}
	return &mcp.StreamableClientTransport{Endpoint: s.URL, HTTPClient: client, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: 1 << 20}, base.CloseIdleConnections, nil
}
