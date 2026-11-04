package webui

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/mcpclient"
)

func (s *Server) mcpConfig(ctx context.Context) (config.App, int64, bool, error) {
	values, rows, err := s.Settings.LoadResolved(ctx, s.Settings.Scope())
	if err != nil {
		return config.App{}, 0, false, err
	}
	cfg, err := s.Base.WithSettings(values)
	if err != nil {
		return cfg, 0, false, err
	}
	for _, r := range rows {
		if r.Name == "mcp.servers" && r.Scope == s.Settings.Scope() {
			return cfg, r.Version, true, nil
		}
	}
	return cfg, 0, false, nil
}
func (s *Server) mcpServers(w http.ResponseWriter, r *http.Request) {
	var err error
	s, err = s.mcpScope(r.URL.Query().Get("scope"))
	if err != nil {
		result(w, err)
		return
	}
	cfg, version, overridden, err := s.mcpConfig(r.Context())
	if err != nil {
		result(w, err)
		return
	}
	rows := []map[string]any{}
	for _, server := range cfg.MCPServers {
		rows = append(rows, map[string]any{"definition": server.Definition(false), "has_token": server.Token.Reveal() != "", "has_env": server.Env.Reveal() != "null" && server.Env.Reveal() != "{}" && server.Env.Reveal() != ""})
	}
	aliases := []string{}
	for _, b := range cfg.Bots {
		aliases = append(aliases, b.Name)
	}
	send(w, map[string]any{"scope": s.Settings.Scope(), "servers": rows, "version": version, "overridden": overridden, "bots": aliases, "restart_required": false})
}
func (s *Server) putMCP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope        string               `json:"scope"`
		Definition   config.MCPDefinition `json:"definition"`
		Version      int64                `json:"version"`
		ReplaceToken bool                 `json:"replace_token"`
		ReplaceEnv   bool                 `json:"replace_env"`
	}
	if !read(w, r, &in) {
		return
	}
	var scopeErr error
	s, scopeErr = s.mcpScope(in.Scope)
	if scopeErr != nil {
		result(w, scopeErr)
		return
	}
	cfg, version, _, err := s.mcpConfig(r.Context())
	if err != nil {
		result(w, err)
		return
	}
	if in.Version != version {
		result(w, errors.New("MCP configuration version conflict"))
		return
	}
	candidate := in.Definition.Server()
	found := false
	for i, old := range cfg.MCPServers {
		if old.Name == candidate.Name {
			if !in.ReplaceToken {
				candidate.Token = old.Token
			}
			if !in.ReplaceEnv {
				candidate.Env = old.Env
			}
			cfg.MCPServers[i] = candidate
			found = true
			break
		}
	}
	if !found {
		cfg.MCPServers = append(cfg.MCPServers, candidate)
	}
	if err = cfg.Validate(); err != nil {
		result(w, err)
		return
	}
	result(w, s.Settings.Put(r.Context(), "mcp.servers", config.EncodeMCP(cfg.MCPServers), version))
}
func (s *Server) deleteMCP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope   string `json:"scope"`
		Name    string `json:"name"`
		Version int64  `json:"version"`
	}
	if !read(w, r, &in) {
		return
	}
	var scopeErr error
	s, scopeErr = s.mcpScope(in.Scope)
	if scopeErr != nil {
		result(w, scopeErr)
		return
	}
	cfg, version, _, err := s.mcpConfig(r.Context())
	if err != nil {
		result(w, err)
		return
	}
	if in.Version != version {
		result(w, errors.New("MCP configuration version conflict"))
		return
	}
	next := slices.DeleteFunc(cfg.MCPServers, func(server config.MCPServer) bool { return server.Name == in.Name })
	result(w, s.Settings.Put(r.Context(), "mcp.servers", config.EncodeMCP(next), version))
}
func (s *Server) probeMCP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Scope string `json:"scope"`
		Name  string `json:"name"`
	}
	if !read(w, r, &in) {
		return
	}
	var scopeErr error
	s, scopeErr = s.mcpScope(in.Scope)
	if scopeErr != nil {
		result(w, scopeErr)
		return
	}
	cfg, _, _, err := s.mcpConfig(r.Context())
	if err != nil {
		result(w, err)
		return
	}
	for _, server := range cfg.MCPServers {
		if server.Name == in.Name {
			// Administrator discovery only; this does not enable tools for the agent.
			server.Enabled = true
			server.Bots = []string{"probe"}
			server.Tools = []string{"*"}
			provider := mcpclient.Provider{Config: cfg, AgentID: cfg.AgentID, Bots: map[int64]string{1: "probe"}, Servers: []config.MCPServer{server}}
			ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
			defer cancel()
			session, err := provider.Open(ctx, chat.Scope{AgentID: cfg.AgentID, BotID: 1, ChatID: 1})
			if err != nil {
				result(w, err)
				return
			}
			defer session.Close()
			send(w, map[string]any{"tools": session.Tools()})
			return
		}
	}
	http.Error(w, "Unknown MCP server", 404)
}

func (s *Server) mcpScope(scope string) (*Server, error) {
	settings, err := s.Settings.InScope(scope)
	if err != nil {
		return nil, err
	}
	if settings.Scope() == "api" {
		return nil, errors.New("MCP definitions belong to shared or worker scope")
	}
	copy := *s
	copy.Settings = settings
	return &copy, nil
}
