package storage

import (
	"context"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"testing"
)

func TestAuditAtomicity(t *testing.T) {
	db, _ := queueDatabases(t, config.SQLite)
	ctx := context.Background()
	err := db.Transaction(ctx, func(tx *Database) error {
		if err := tx.orm.Exec("INSERT INTO shared_knowledge(agent_id,content) VALUES ('test','must rollback')").Error; err != nil {
			return err
		}
		return tx.Audit(ctx, "", "PUT test", "{}", 200)
	})
	if err == nil {
		t.Fatal("invalid audit accepted")
	}
	value, _, err := db.Agent().Knowledge(ctx, "test")
	if err != nil || value != "" {
		t.Fatal("mutation escaped transaction", err)
	}
	err = db.Transaction(ctx, func(tx *Database) error {
		if err := tx.Agent().PutKnowledge(ctx, "test", "saved", 0); err != nil {
			return err
		}
		return tx.Audit(ctx, "admin", "POST /api/knowledge", "{}", 200, "[]", `[{"content":"saved"}]`)
	})
	if err != nil {
		t.Fatal(err)
	}
	page, err := db.Agent().AdminPage(ctx, "audit", "", 10, chat.Scope{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal("audit missing", err)
	}
}
