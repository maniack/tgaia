package webui

import (
	"bytes"
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/storage"
)

func skillsServer(t *testing.T) (*Server, *storage.Database) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(filepath.Join(t.TempDir(), "webui-skills.db"))}, ConnectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Bots().Register(ctx, "test", 1, "TOKEN"); err != nil {
		t.Fatal(err)
	}
	return &Server{DB: db}, db
}

func TestSkillsAPIListsCatalogAndVersions(t *testing.T) {
	server, db := skillsServer(t)
	ctx := context.Background()
	first, err := db.Agent().SaveSkill(ctx, "test", 1, 7, "daily_brief", "Build a brief", `def run(input_json): return input_json`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Agent().FinishSkillTest(ctx, first, `{}`, `ok`, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Agent().SaveSkill(ctx, "test", 1, 7, "daily_brief", "Build a better brief", `def run(input_json): return "v2"`); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	server.skills(w, httptest.NewRequest("GET", "/api/skills?agent_id=test&bot_id=1", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"name":"daily_brief"`) || !strings.Contains(w.Body.String(), `"version":2`) {
		t.Fatalf("catalog response %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	server.skills(w, httptest.NewRequest("GET", "/api/skills?agent_id=test&bot_id=1&name=daily_brief", nil))
	if w.Code != 200 || strings.Count(w.Body.String(), `"name":"daily_brief"`) != 2 || !strings.Contains(w.Body.String(), `"status":"active"`) {
		t.Fatalf("versions response %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	server.skills(w, httptest.NewRequest("GET", "/api/skills?agent_id=test&bot_id=1&name=../bad", nil))
	if w.Code != 400 {
		t.Fatalf("invalid skill query returned %d", w.Code)
	}
}

func TestVirtualFilesAPIEnforcesNamespaceScope(t *testing.T) {
	server, db := skillsServer(t)
	ctx := context.Background()
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	private := "private note"
	shared := "shared note"
	if err := db.Agent().ApplyVirtualChanges(ctx, scope, 7, []storage.VirtualFileChange{{Path: "/user/note.txt", Content: &private}, {Path: "/chat/shared.txt", Content: &shared}}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	server.virtualFiles(w, httptest.NewRequest("GET", "/api/vfs?agent_id=test&bot_id=1&chat_id=100&namespace=user&user_id=7", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"path":"note.txt"`) || strings.Contains(w.Body.String(), "shared note") {
		t.Fatalf("user listing %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	server.virtualFiles(w, httptest.NewRequest("GET", "/api/vfs?agent_id=test&bot_id=1&chat_id=100&user_id=7&path=/user/note.txt", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "private note") {
		t.Fatalf("file response %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	server.virtualFiles(w, httptest.NewRequest("GET", "/api/vfs?agent_id=test&bot_id=1&chat_id=100&user_id=8&path=/user/note.txt", nil))
	if w.Code != 404 {
		t.Fatalf("foreign user file returned %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	server.virtualFiles(w, httptest.NewRequest("GET", "/api/vfs?agent_id=test&bot_id=1&chat_id=100&namespace=host&user_id=7", nil))
	if w.Code != 400 {
		t.Fatalf("invalid namespace returned %d", w.Code)
	}
}

func TestStatusIncludesConfiguredCoreCapabilities(t *testing.T) {
	server, _ := skillsServer(t)
	server.Config = config.App{LLMModel: "local-model", TelegramMode: "mock", OIDCMode: "mock", MemoryWindow: time.Hour, MemoryInterval: time.Minute}
	server.RuntimeStatus = func() any {
		return map[string]any{"state": "applied", "bot_profiles": []map[string]any{{"bot_id": 42, "bio": "Arctic helper", "context_included": true}}}
	}
	w := httptest.NewRecorder()
	server.status(w, httptest.NewRequest("GET", "/api/status", nil))
	if w.Code != 200 {
		t.Fatalf("status response %d: %s", w.Code, w.Body.String())
	}
	for _, want := range []string{"Language model", "local-model", "Telegram transport", "Scheduler", "Memory compaction", "Authentication", "Agent-written skills", "Virtual filesystem", "Arctic helper", "context_included"} {
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("status response missing %q: %s", want, w.Body.String())
		}
	}
}

func TestDemoMessageCarriesTelegramProfileAndReplyTarget(t *testing.T) {
	server, db := skillsServer(t)
	server.Config = config.App{AgentID: "test", Bots: []config.Bot{{ID: 1}}}
	body := `{"bot_id":1,"chat_id":100,"user_id":7,"reply_to_message_id":42,"name":"Ada Lovelace","username":"ada","language_code":"en","is_premium":true,"text":"reply"}`
	w := httptest.NewRecorder()
	server.demoMessage(w, httptest.NewRequest("POST", "/api/demo-message", bytes.NewBufferString(body)))
	if w.Code != 200 {
		t.Fatalf("demo response %d: %s", w.Code, w.Body.String())
	}
	ctx, err := db.Agent().Context(context.Background(), chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}, 0, 10)
	if err != nil || len(ctx.Messages) != 1 || ctx.Messages[0].ReplyToMessageID != 42 || ctx.Messages[0].Name != "Ada Lovelace" || ctx.Messages[0].Username != "ada" {
		t.Fatalf("demo message lost profile/reply data: %+v %v", ctx.Messages, err)
	}
	profile, err := db.Agent().TelegramProfile(context.Background(), "test", 7)
	if err != nil || profile.LanguageCode != "en" || !profile.IsPremium {
		t.Fatalf("demo profile was not persisted: %+v %v", profile, err)
	}
}
