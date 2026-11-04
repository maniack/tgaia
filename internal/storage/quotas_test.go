package storage

import (
	"context"
	"errors"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"sync"
	"sync/atomic"
	"testing"
)

func TestQuotaSharedAcrossReplicas(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			db, peer := queueDatabases(t, driver)
			ctx := context.Background()
			sc := chat.Scope{AgentID: "test", BotID: 1, ChatID: 42}
			var accepted atomic.Int64
			var wg sync.WaitGroup
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					d := db
					if i%2 == 0 {
						d = peer
					}
					err := d.Agent().ReserveCall(ctx, sc, "llm", 3)
					if err == nil {
						accepted.Add(1)
					} else if !errors.Is(err, ErrQuota) {
						t.Error(err)
					}
				}(i)
			}
			wg.Wait()
			if accepted.Load() != 3 {
				t.Fatal("quota exceeded", accepted.Load())
			}
			sc.ThreadID = 8
			if err := peer.Agent().ReserveCall(ctx, sc, "llm", 3); !errors.Is(err, ErrQuota) {
				t.Fatal("topic bypass", err)
			}
			if err := peer.Agent().ReserveCall(ctx, sc, "compaction", 1); err != nil {
				t.Fatal("compaction shares response quota", err)
			}
			if err := db.orm.Exec("UPDATE usage_quotas SET bucket=0 WHERE category='llm'").Error; err != nil {
				t.Fatal(err)
			}
			if err := peer.Agent().ReserveCall(ctx, sc, "llm", 3); err != nil {
				t.Fatal("window did not reset", err)
			}
		})
	}
}
