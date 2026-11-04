package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Credentials are carried only by Secret; UI uses a separate redacted DTO.
type MCPServer struct {
	Name      string
	Enabled   bool
	Transport string
	URL       string
	Command   string
	Args      []string
	Token     Secret
	Env       Secret   // JSON object, explicitly supplied subprocess environment only.
	Bots      []string // Configuration aliases, never model-provided bot IDs.
	Tools     []string
}
type mcpServerSchema struct {
	Enabled   string `yaml:"enabled"`
	Transport string `yaml:"transport"`
	URL       string `yaml:"url"`
	Command   string `yaml:"command"`
	Args      string `yaml:"args"`
	Token     string `yaml:"token"`
	Env       string `yaml:"env"`
	Bots      string `yaml:"bots"`
	Tools     string `yaml:"tools"`
}

// MCPDefinition is for the private configuration codec, not API responses or logs.
type MCPDefinition struct {
	Name      string            `json:"name"`
	Enabled   bool              `json:"enabled"`
	Transport string            `json:"transport"`
	URL       string            `json:"url"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Token     string            `json:"token,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Bots      []string          `json:"bots"`
	Tools     []string          `json:"tools"`
}

func (s MCPServer) Definition(secrets bool) MCPDefinition {
	d := MCPDefinition{Name: s.Name, Enabled: s.Enabled, Transport: s.Transport, URL: s.URL, Command: s.Command, Args: s.Args, Bots: s.Bots, Tools: s.Tools}
	if secrets {
		d.Token = s.Token.Reveal()
		_ = json.Unmarshal([]byte(s.Env.Reveal()), &d.Env)
	}
	return d
}
func (d MCPDefinition) Server() MCPServer {
	env, _ := json.Marshal(d.Env)
	return MCPServer{Name: d.Name, Enabled: d.Enabled, Transport: d.Transport, URL: d.URL, Command: d.Command, Args: d.Args, Token: NewSecret(d.Token), Env: NewSecret(string(env)), Bots: d.Bots, Tools: d.Tools}
}
func EncodeMCP(servers []MCPServer) string {
	defs := make([]MCPDefinition, 0, len(servers))
	for _, s := range servers {
		defs = append(defs, s.Definition(true))
	}
	b, _ := json.Marshal(defs)
	return string(b)
}
func DecodeMCP(value string) ([]MCPServer, error) {
	var defs []MCPDefinition
	dec := json.NewDecoder(strings.NewReader(value))
	dec.DisallowUnknownFields()
	if dec.Decode(&defs) != nil || dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid MCP server definitions")
	}
	result := make([]MCPServer, 0, len(defs))
	for _, d := range defs {
		result = append(result, d.Server())
	}
	return result, nil
}

var mcpName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,39}$`)

func (c App) validateMCP() error {
	if len(c.MCPServers) > 16 {
		return errors.New("at most 16 MCP servers are allowed")
	}
	seen := map[string]bool{}
	bots := map[string]bool{"*": true}
	for _, b := range c.Bots {
		bots[b.Name] = true
	}
	for _, s := range c.MCPServers {
		if !mcpName.MatchString(s.Name) || seen[s.Name] {
			return errors.New("invalid or duplicate MCP server name")
		}
		seen[s.Name] = true
		if s.Transport != "http" && s.Transport != "sse" && s.Transport != "stdio" {
			return errors.New("MCP transport must be http, sse or stdio")
		}
		if len(s.Tools) > 128 || len(s.Bots) > 64 || len(s.Args) > 64 {
			return errors.New("MCP configuration exceeds limits")
		}
		for _, b := range s.Bots {
			if !bots[b] {
				return errors.New("MCP bot alias is unknown")
			}
		}
		for _, t := range s.Tools {
			if t == "" || len(t) > 256 {
				return errors.New("invalid MCP tool allowlist")
			}
		}
		if s.Enabled && (len(s.Tools) == 0 || len(s.Bots) == 0) {
			return errors.New("enabled MCP servers require bot and tool allowlists")
		}
		if s.Transport == "stdio" {
			if !filepath.IsAbs(s.Command) || s.URL != "" || s.Token.Reveal() != "" {
				return errors.New("stdio MCP requires an absolute executable and no URL/token")
			}
			var env map[string]string
			if json.Unmarshal([]byte(s.Env.Reveal()), &env) != nil {
				return errors.New("invalid MCP environment")
			}
			for k, v := range env {
				if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
					return errors.New("invalid MCP environment")
				}
			}
			for _, arg := range s.Args {
				if strings.ContainsRune(arg, 0) {
					return errors.New("invalid MCP argument")
				}
			}
		} else {
			u, err := url.Parse(s.URL)
			if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
				return errors.New("invalid MCP URL; credentials belong in token")
			}
			if s.Command != "" || len(s.Args) > 0 {
				return errors.New("remote MCP cannot specify a command")
			}
			if strings.ContainsAny(s.Token.Reveal(), "\r\n") {
				return errors.New("invalid MCP bearer token")
			}
		}
	}
	return nil
}
func parseMCPFile(values map[string]mcpServerSchema) ([]MCPServer, error) {
	var result []MCPServer
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		v := values[name]
		d := MCPDefinition{Name: name, Transport: v.Transport, URL: v.URL, Command: v.Command, Token: v.Token}
		if d.Transport == "" {
			d.Transport = "http"
		}
		if v.Enabled != "" && v.Enabled != "true" && v.Enabled != "false" {
			return nil, errors.New("invalid MCP enabled value")
		}
		d.Enabled = v.Enabled == "true"
		for _, p := range []struct {
			value string
			dest  any
		}{{v.Args, &d.Args}, {v.Env, &d.Env}, {v.Bots, &d.Bots}, {v.Tools, &d.Tools}} {
			if p.value != "" {
				dec := json.NewDecoder(bytes.NewBufferString(p.value))
				if dec.Decode(p.dest) != nil || dec.Decode(new(any)) != io.EOF {
					return nil, errors.New("invalid MCP JSON configuration field")
				}
			}
		}
		result = append(result, d.Server())
	}
	return result, nil
}
