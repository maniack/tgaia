package storage

import (
	"context"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
)

// Compaction runs per batch and never twice for one chat at the same time.
func TestCompactionBatchingAndScopeDedup(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 26)
	ctx := context.Background()
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	add := func(id int64, age time.Duration) {
		t.Helper()
		if err := db.Agent().Accept(ctx, agent.Incoming{Scope: scope, UserID: 7, UpdateID: id, MessageID: id, Text: "m", SentAt: time.Now().Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	compactJobs := func() int64 {
		var n int64
		db.orm.Table("queue_jobs").Where("kind='compact'").Count(&n)
		return n
	}
	policy := CompactionPolicy{MinMessages: 3, MaxUncompact: 5}
	for id := int64(1); id <= 2; id++ {
		add(id, 2*time.Hour)
	}
	if err := db.Agent().Tick(ctx, time.Hour, policy); err != nil {
		t.Fatal(err)
	}
	if compactJobs() != 0 {
		t.Fatal("compaction started below the batch size")
	}
	add(3, 2*time.Hour)
	if err := db.Agent().Tick(ctx, time.Hour, policy); err != nil {
		t.Fatal(err)
	}
	if compactJobs() != 1 {
		t.Fatal("batch of three did not start compaction")
	}
	// More messages age out while the job is still queued: no second job.
	add(4, 2*time.Hour)
	add(5, 2*time.Hour)
	add(6, 2*time.Hour)
	if err := db.Agent().Tick(ctx, time.Hour, policy); err != nil {
		t.Fatal(err)
	}
	if compactJobs() != 1 {
		t.Fatal("overlapping compaction enqueued while one is pending")
	}
	job, err := db.Queue().Claim(ctx, []string{"compact"}, time.Minute)
	if err != nil || job == nil {
		t.Fatal("compact job not claimable", err)
	}
	if err := db.Agent().SaveSummary(ctx, *job, 0, 3, "memory through 3", 0); err != nil {
		t.Fatal(err)
	}
	memory, err := db.Agent().Context(ctx, scope, 0, 50)
	if err != nil || memory.Summary.ThroughAt == 0 || memory.Summary.PendingCount != 3 {
		t.Fatalf("summary status is incomplete: %+v, %v", memory.Summary, err)
	}
	if err := db.Queue().Complete(ctx, job.Receipt); err != nil {
		t.Fatal(err)
	}
	if err := db.Agent().Tick(ctx, time.Hour, policy); err != nil {
		t.Fatal(err)
	}
	if compactJobs() != 2 {
		t.Fatal("remaining batch not compacted after the previous job finished")
	}
	// Fresh messages are not eligible, but too many unsummarized ones force a
	// compaction of whatever has already aged out.
	db2, _ := databasesAtVersion(t, config.SQLite, 26)
	fresh := func(id int64, age time.Duration) {
		t.Helper()
		if err := db2.Agent().Accept(ctx, agent.Incoming{Scope: scope, UserID: 7, UpdateID: id, MessageID: id, Text: "m", SentAt: time.Now().Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	fresh(1, 2*time.Hour)
	for id := int64(2); id <= 7; id++ {
		fresh(id, time.Minute)
	}
	if err := db2.Agent().Tick(ctx, time.Hour, policy); err != nil {
		t.Fatal(err)
	}
	var n int64
	db2.orm.Table("queue_jobs").Where("kind='compact'").Count(&n)
	if n != 1 {
		t.Fatal("unsummarized backlog above the limit did not trigger compaction")
	}
	// Default policy keeps the historical per-message behaviour for callers that
	// pass none (tests), so existing expectations hold.
	db3, _ := databasesAtVersion(t, config.SQLite, 26)
	if err := db3.Agent().Accept(ctx, agent.Incoming{Scope: scope, UserID: 7, UpdateID: 1, MessageID: 1, Text: "m", SentAt: time.Now().Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := db3.Agent().Tick(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	db3.orm.Table("queue_jobs").Where("kind='compact'").Count(&n)
	if n != 1 {
		t.Fatal("default policy must compact a single aged message")
	}
}
