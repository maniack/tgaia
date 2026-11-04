package webui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/storage"
)

func TestMCPCRUDAndSecretRedaction(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(filepath.Join(t.TempDir(), "mcp.db"))}, ConnectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	settings, err := db.Settings(config.NewSecret(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	base := config.App{AgentID: "a", Listen: "127.0.0.1:8080", PublicURL: "http://127.0.0.1:8080", Role: "all", OIDCMode: "mock", OIDCClientID: "test", MockLLM: true, TelegramMode: "mock", MemoryWindow: time.Hour, MemoryInterval: time.Second, Bots: []config.Bot{{Name: "assistant", ID: 1, Username: "bot"}}, MCPServers: []config.MCPServer{{Name: "existing", Transport: "http", URL: "http://127.0.0.1:1234/mcp", Token: config.NewSecret("original-secret")}}}
	server := &Server{DB: db, Settings: settings, Base: base, Config: base}
	post := func(body string) int {
		w := httptest.NewRecorder()
		server.putMCP(w, httptest.NewRequest("POST", "/api/mcp", strings.NewReader(body)))
		return w.Code
	}
	if code := post(`{"version":0,"definition":{"name":"new","transport":"http","url":"http://127.0.0.1:1234/mcp","token":"new-secret","bots":["assistant"],"tools":["echo"],"enabled":true}}`); code != 200 {
		t.Fatalf("create: %d", code)
	}
	if code := post(`{"version":0,"definition":{"name":"new","transport":"http","url":"http://127.0.0.1:1234/mcp"}}`); code == 200 {
		t.Fatal("stale update accepted")
	}
	if code := post(`{"version":1,"definition":{"name":"new","transport":"http","url":"http://127.0.0.1:1234/mcp","bots":["assistant"],"tools":["echo"],"enabled":true}}`); code != 200 {
		t.Fatalf("edit: %d", code)
	}
	w := httptest.NewRecorder()
	server.mcpServers(w, httptest.NewRequest("GET", "/api/mcp", nil))
	if strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), `"has_token":true`) {
		t.Fatal("credentials leaked or disappeared")
	}
	cfg, version, _, err := server.mcpConfig(ctx)
	if err != nil || version != 2 || cfg.MCPServers[1].Token.Reveal() != "new-secret" {
		t.Fatal("blank edit erased secret")
	}
	var view map[string]any
	if json.Unmarshal(w.Body.Bytes(), &view) != nil {
		t.Fatal("invalid view")
	}
	w = httptest.NewRecorder()
	server.deleteMCP(w, httptest.NewRequest("DELETE", "/api/mcp", strings.NewReader(`{"name":"existing","version":2}`)))
	if w.Code != 200 || base.MCPServers[0].Name != "existing" {
		t.Fatal("delete mutated bootstrap")
	}
	_, rows, err := settings.Load(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Secret || rows[0].Value != "" {
		t.Fatal("MCP setting not redacted/encrypted")
	}
}
