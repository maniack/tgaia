package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPFileAndOverride(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	content := `app:
  agent_id: test
  listen: 127.0.0.1:8080
  public_url: http://127.0.0.1:8080
  role: all
  llm:
    mock: "true"
  oidc:
    mode: mock
    client_id: test
  telegram:
    mode: mock
    webhook_listen: 127.0.0.1:8081
    bots:
      one:
        id: "1"
        username: test
  mcp:
    servers:
      docs:
        enabled: "true"
        transport: http
        url: http://127.0.0.1:9000/mcp
        token: hidden-secret
        bots: '["one"]'
        tools: '["search"]'
`
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := (&File{Path: file}).App()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Token.Reveal() != "hidden-secret" {
		t.Fatal("file MCP not read")
	}
	if cfg.TelegramWebhookListen != "127.0.0.1:8081" {
		t.Fatal("Telegram webhook listener not read")
	}
	next, err := cfg.WithSettings(map[string]string{"mcp.servers": "[]"})
	if err != nil || len(next.MCPServers) != 0 || len(cfg.MCPServers) != 1 {
		t.Fatal("override/base aliasing")
	}
	if !SecretSetting("mcp.servers") {
		t.Fatal("MCP credentials not protected")
	}
	invalid := strings.Replace(EncodeMCP(cfg.MCPServers), `"one"`, `"unknown"`, 1)
	if _, err := cfg.WithSettings(map[string]string{"mcp.servers": invalid}); err == nil {
		t.Fatal("unknown bot allowed")
	}
}
