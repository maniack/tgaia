package skills_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/skills"
	"github.com/maniack/tgaia/internal/storage"
)

func skillFixture(t *testing.T) (*storage.Database, chat.Scope) {
	t.Helper()
	ctx := context.Background()
	db, err := storage.Open(ctx, config.Config{Database: config.Database{Driver: config.SQLite, DSN: config.NewSecret(filepath.Join(t.TempDir(), "skills.db"))}, ConnectTimeout: time.Second})
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
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	if err := db.Agent().Accept(ctx, agent.Incoming{Scope: scope, UpdateID: 1, MessageID: 1, UserID: 7, Name: "Ada", ChatType: "private", Title: "test", Text: "hello", SentAt: time.Now(), Respond: false}); err != nil {
		t.Fatal(err)
	}
	return db, scope
}

func TestRuntimeUsesOnlyTransactionalVirtualFilesystem(t *testing.T) {
	db, scope := skillFixture(t)
	ctx := context.Background()
	runtime := skills.Runtime{Store: db.Agent()}
	source := `def run(input_json):
    vfs_write("/user/preferences.txt", input_json)
    vfs_write("/chat/notes.txt", "shared")
    vfs_write("/bot/cache.txt", "bot")
    vfs_write("/tmp/work.txt", "temporary")
    return vfs_read("/user/preferences.txt") + ":" + vfs_read("/tmp/work.txt")`
	output, err := runtime.Run(ctx, scope, 7, source, `{"language":"ru"}`, true)
	if err != nil || output != `{"language":"ru"}:temporary` {
		t.Fatalf("output=%q err=%v", output, err)
	}
	for _, path := range []string{"/user/preferences.txt", "/chat/notes.txt", "/bot/cache.txt"} {
		if _, err := db.Agent().ReadVirtualFile(ctx, scope, 7, path); err != nil {
			t.Fatalf("%s not committed: %v", path, err)
		}
	}
	if _, err := db.Agent().ReadVirtualFile(ctx, scope, 7, "/tmp/work.txt"); err == nil {
		t.Fatal("temporary file escaped into the database")
	}
	if _, err := db.Agent().ReadVirtualFile(ctx, scope, 8, "/user/preferences.txt"); err == nil {
		t.Fatal("another user read private virtual storage")
	}
}

func TestRuntimeTestRunDoesNotCommitAndRejectsHostAccess(t *testing.T) {
	db, scope := skillFixture(t)
	runtime := skills.Runtime{Store: db.Agent()}
	ctx := context.Background()
	if _, err := runtime.Run(ctx, scope, 7, `def run(input_json):
    vfs_write("/chat/draft.txt", "draft")
    return "ok"`, `{}`, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Agent().ReadVirtualFile(ctx, scope, 7, "/chat/draft.txt"); err == nil {
		t.Fatal("test run committed a file")
	}
	for _, source := range []string{
		`load("os", "system")
def run(input_json): return system("id")`,
		`def run(input_json): return vfs_read("/chat/../../etc/passwd")`,
	} {
		if _, err := runtime.Run(ctx, scope, 7, source, `{}`, true); err == nil || (!strings.Contains(err.Error(), "load") && !strings.Contains(err.Error(), "virtual path")) {
			t.Fatalf("unsafe source was not rejected: %v", err)
		}
	}
}
