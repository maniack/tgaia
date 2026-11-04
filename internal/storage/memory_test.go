package storage

import (
	"context"
	"testing"

	"github.com/maniack/tgaia/internal/config"
)

// Migration 018 collapses per-topic summaries into one chat-wide summary,
// keeping the most advanced checkpoint of each chat.
func TestChatMemoryMigrationCollapsesTopicSummaries(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 17)
	ctx := context.Background()
	// The fixture already seeds agent "test" with bot 1.
	if err := db.orm.Exec("INSERT INTO telegram_bots(agent_id,bot_id,credential_ref) VALUES ('test',1,'ref') ON CONFLICT DO NOTHING").Error; err != nil {
		t.Fatal(err)
	}
	for _, chat := range []int64{100, 200} {
		if err := db.orm.Exec("INSERT INTO chat_directory(agent_id,bot_id,chat_id,title,kind) VALUES ('test',1,?,'t','supergroup')", chat).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.orm.Exec("INSERT INTO memory_summaries(agent_id,bot_id,chat_id,thread_id,through_id,content) VALUES ('test',1,100,0,5,'general'),('test',1,100,10,9,'topic ten'),('test',1,100,20,9,'topic twenty'),('test',1,200,7,3,'other chat')").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		ChatID, ThreadID, ThroughID int64
		Content                     string
	}
	if err := db.orm.Table("memory_summaries").Order("chat_id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ChatID != 100 || rows[0].ThreadID != 0 || rows[0].ThroughID != 9 || rows[0].Content != "topic ten" || rows[1].ChatID != 200 || rows[1].ThreadID != 0 || rows[1].Content != "other chat" {
		t.Fatalf("unexpected summaries after migration: %+v", rows)
	}
	if err := db.Agent().Ready(ctx); err != nil {
		t.Fatal(err)
	}
}
