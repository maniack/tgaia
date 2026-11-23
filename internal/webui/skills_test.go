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
	first, err := db.Agent().SaveSkill(ctx, "test", 1, 7, "daily_brief", "Build a brief", `def run(input_json): return input_json`, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Agent().FinishSkillTest(ctx, first, `{}`, `ok`, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Agent().SaveSkill(ctx, "test", 1, 7, "daily_brief", "Build a better brief", `def run(input_json): return "v2"`, false); err != nil {
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

func TestVirtualFilesAPIScopedToChat(t *testing.T) {
	server, db := skillsServer(t)
	ctx := context.Background()
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	otherScope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 200}
	note := "chat note"
	other := "other chat note"
	if err := db.Agent().ApplyVirtualChanges(ctx, scope, 7, []storage.VirtualFileChange{{Path: "/note.txt", Content: &note}}); err != nil {
		t.Fatal(err)
	}
	if err := db.Agent().ApplyVirtualChanges(ctx, otherScope, 7, []storage.VirtualFileChange{{Path: "/note.txt", Content: &other}}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	server.virtualFiles(w, httptest.NewRequest("GET", "/api/vfs?agent_id=test&bot_id=1&chat_id=100", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"path":"note.txt"`) || strings.Contains(w.Body.String(), "other chat note") {
		t.Fatalf("chat listing %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	server.virtualFiles(w, httptest.NewRequest("GET", "/api/vfs?agent_id=test&bot_id=1&chat_id=100&path=/note.txt", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "chat note") {
		t.Fatalf("file response %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	server.virtualFiles(w, httptest.NewRequest("GET", "/api/vfs?agent_id=test&bot_id=1&chat_id=100&path=/nonexistent.txt", nil))
	if w.Code != 404 {
		t.Fatalf("missing file returned %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteSkillAndVirtualFile(t *testing.T) {
	server, db := skillsServer(t)
	ctx := context.Background()
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}

	skill, err := db.Agent().SaveSkill(ctx, "test", 1, 7, "my_skill", "A skill", `def run(input_json): return "ok"`, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Agent().FinishSkillTest(ctx, skill, `{}`, `ok`, ""); err != nil {
		t.Fatal(err)
	}

	content := "hello"
	if err := db.Agent().ApplyVirtualChanges(ctx, scope, 7, []storage.VirtualFileChange{{Path: "/hello.txt", Content: &content}}); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	server.deleteSkill(w, httptest.NewRequest("DELETE", "/api/skills", bytes.NewBufferString(`{"agent_id":"test","bot_id":1,"name":"my_skill"}`)))
	if w.Code != 200 {
		t.Fatalf("skill delete returned %d: %s", w.Code, w.Body.String())
	}
	if _, err := db.Agent().Skill(ctx, "test", 1, "my_skill", false); err == nil {
		t.Fatal("skill still exists after delete")
	}

	w = httptest.NewRecorder()
	server.deleteVirtualFile(w, httptest.NewRequest("DELETE", "/api/vfs", bytes.NewBufferString(`{"agent_id":"test","bot_id":1,"chat_id":100,"path":"/hello.txt"}`)))
	if w.Code != 200 {
		t.Fatalf("vfs delete returned %d: %s", w.Code, w.Body.String())
	}
	files, err := db.Agent().ListVirtualFiles(ctx, scope, "")
	if err != nil || len(files) != 0 {
		t.Fatalf("file still exists after delete: %v %v", files, err)
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
