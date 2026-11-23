package storage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/queue"
)

// Compaction runs per batch and never twice for one chat at the same time.
func TestCompactionBatchingAndScopeDedup(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 42)
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
	if _, err := db.Agent().SaveSummary(ctx, *job, 0, 3, "memory through 3", 0); err != nil {
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
	db2, _ := databasesAtVersion(t, config.SQLite, 42)
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
	db3, _ := databasesAtVersion(t, config.SQLite, 42)
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

// When the pending backlog exceeds MaxUncompact and no messages have aged past
// the time window, Tick must still enqueue one compaction that covers all but
// the last MinMessages messages (count-based through_id, not time-based).
func TestForcedCompactionUsesCountBasedThroughID(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 42)
	ctx := context.Background()
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	for id := int64(1); id <= 8; id++ {
		if err := db.Agent().Accept(ctx, agent.Incoming{Scope: scope, UserID: 7, UpdateID: id, MessageID: id, Text: "m", SentAt: time.Now().Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	policy := CompactionPolicy{MinMessages: 3, MaxUncompact: 5}
	if err := db.Agent().Tick(ctx, time.Hour, policy); err != nil {
		t.Fatal(err)
	}
	var jobs []struct{ Payload string }
	db.orm.Table("queue_jobs").Where("kind='compact'").Select("payload").Find(&jobs)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 compact job, got %d", len(jobs))
	}
	var jp agent.JobPayload
	if err := json.Unmarshal([]byte(jobs[0].Payload), &jp); err != nil {
		t.Fatal(err)
	}
	// pending=8, MinMessages=3: compact 8-3=5 messages, through_id=id of 5th message=5.
	if jp.ThroughID != 5 {
		t.Fatalf("expected count-based through_id=5 (8-3 messages), got %d", jp.ThroughID)
	}
}

// Tick must not enqueue a second run for a recurring task while the previous
// run is still pending or leased (coalesce/skip-if-behind).
func TestTickCoalescesRecurringTask(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 42)
	ctx := context.Background()
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	if err := db.Agent().Accept(ctx, agent.Incoming{Scope: scope, UserID: 7, UpdateID: 1, MessageID: 1, Text: "hi", SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	task := agent.Task{
		ID: "task-coalesce-1", AgentID: "test", BotID: 1, ChatID: 100, ThreadID: 0,
		UserID: 7, Prompt: "ping", RunAt: time.Now().Add(-time.Minute).UnixMilli(),
		IntervalMS: 60000, Active: true,
	}
	if err := db.Agent().PutTask(ctx, task, 0); err != nil {
		t.Fatal(err)
	}
	// First tick: job is enqueued.
	if err := db.Agent().Tick(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.orm.Table("queue_jobs").Where("kind='respond' AND dedupe_key LIKE 'task:%'").Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 task job after first tick, got %d", count)
	}
	// Second tick: previous run still pending — must not enqueue another.
	if err := db.Agent().Tick(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	db.orm.Table("queue_jobs").Where("kind='respond' AND dedupe_key LIKE 'task:%'").Count(&count)
	if count != 1 {
		t.Fatalf("coalesce failed: expected 1 task job, got %d", count)
	}
}

// CancelTaskJobs moves pending task runs to dead/cancelled and does not touch
// runs that are already leased or completed.
func TestCancelTaskJobs(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 42)
	ctx := context.Background()
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	if err := db.Agent().Accept(ctx, agent.Incoming{Scope: scope, UserID: 7, UpdateID: 1, MessageID: 1, Text: "hi", SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	task := agent.Task{
		ID: "task-cancel-1", AgentID: "test", BotID: 1, ChatID: 100, ThreadID: 0,
		UserID: 7, Prompt: "ping", RunAt: time.Now().Add(-time.Minute).UnixMilli(),
		IntervalMS: 60000, Active: true,
	}
	if err := db.Agent().PutTask(ctx, task, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.Agent().Tick(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	var pending int64
	db.orm.Table("queue_jobs").Where("kind='respond' AND dedupe_key LIKE 'task:%' AND status='pending'").Count(&pending)
	if pending != 1 {
		t.Fatalf("expected 1 pending task job before cancel, got %d", pending)
	}
	if err := db.Agent().CancelTaskJobs(ctx, "test", 1, 100, task.ID); err != nil {
		t.Fatal(err)
	}
	db.orm.Table("queue_jobs").Where("kind='respond' AND dedupe_key LIKE 'task:%' AND status='pending'").Count(&pending)
	if pending != 0 {
		t.Fatalf("pending task job not cancelled, still %d", pending)
	}
	var dead int64
	db.orm.Table("queue_jobs").Where("kind='respond' AND dedupe_key LIKE 'task:%' AND status='dead' AND failure_class='cancelled'").Count(&dead)
	if dead != 1 {
		t.Fatalf("expected 1 dead/cancelled task job, got %d", dead)
	}
}

// TestSessionEpisodes verifies that session episodes are stored, returned by
// Context, and used as the history floor so raw messages load after them.
func TestSessionEpisodes(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 42)
	ctx := context.Background()
	scope := chat.Scope{AgentID: "test", BotID: 1, ChatID: 100}
	add := func(id int64) {
		t.Helper()
		if err := db.Agent().Accept(ctx, agent.Incoming{Scope: scope, UserID: 7, UpdateID: id, MessageID: id, Text: "msg", SentAt: time.Now().Add(-2 * time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	// Add 5 messages.
	for id := int64(1); id <= 5; id++ {
		add(id)
	}

	// Save a background summary covering messages 1-3.
	bgJob, err := db.Queue().Claim(ctx, []string{"compact"}, time.Minute)
	if err != nil || bgJob == nil {
		// No compact job queued yet; create one inline.
		payload, _ := json.Marshal(agent.JobPayload{ThroughID: 3})
		if _, err := db.Queue().Enqueue(ctx, queue.Input{Scope: scope, Key: "compact:test", Kind: "compact", Payload: payload, MaxAttempts: 1}); err != nil {
			t.Fatal(err)
		}
		bgJob, err = db.Queue().Claim(ctx, []string{"compact"}, time.Minute)
		if err != nil || bgJob == nil {
			t.Fatal("could not claim compact job", err)
		}
	}
	if _, err := db.Agent().SaveSummary(ctx, *bgJob, 0, 3, "background: messages 1-3", 0); err != nil {
		t.Fatal(err)
	}
	_ = db.Queue().Complete(ctx, bgJob.Receipt)

	// Save a session episode covering messages 4-5 (period_start = 3, the background through_id).
	sessionPayload, _ := json.Marshal(agent.JobPayload{ThroughID: 5})
	if _, err := db.Queue().Enqueue(ctx, queue.Input{Scope: scope, Key: "compact_session:test", Kind: "compact_session", Payload: sessionPayload, MaxAttempts: 1}); err != nil {
		t.Fatal(err)
	}
	sessionJob, err := db.Queue().Claim(ctx, []string{"compact_session"}, time.Minute)
	if err != nil || sessionJob == nil {
		t.Fatal("could not claim compact_session job", err)
	}
	if err := db.Agent().SaveSessionEpisode(ctx, *sessionJob, 3, 5, "session: messages 4-5"); err != nil {
		t.Fatal(err)
	}
	_ = db.Queue().Complete(ctx, sessionJob.Receipt)

	// Context should return the background summary and the session episode.
	c, err := db.Agent().Context(ctx, scope, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if c.Summary.ThroughID != 3 || c.Summary.Content != "background: messages 1-3" {
		t.Fatalf("wrong background summary: %+v", c.Summary)
	}
	if len(c.Episodes) != 1 || c.Episodes[0].ThroughID != 5 || c.Episodes[0].Content != "session: messages 4-5" {
		t.Fatalf("wrong session episodes: %+v", c.Episodes)
	}
	// No raw messages should be loaded (all 5 are covered by background + session).
	if len(c.Messages) != 0 {
		t.Fatalf("expected no raw messages after episodes, got %d", len(c.Messages))
	}
	// PendingCount counts messages after background (3), so messages 4+5 = 2.
	if c.Summary.PendingCount != 2 {
		t.Fatalf("expected PendingCount=2, got %d", c.Summary.PendingCount)
	}

	// SessionEpisodeCount after background through_id should be 1.
	count, err := db.Agent().SessionEpisodeCount(ctx, scope, 3)
	if err != nil || count != 1 {
		t.Fatalf("SessionEpisodeCount: got %d, err %v", count, err)
	}

	// Add a 6th message: it should appear as the only raw message.
	add(6)
	c2, err := db.Agent().Context(ctx, scope, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(c2.Messages) != 1 || c2.Messages[0].MessageID != 6 {
		t.Fatalf("expected only message 6 as raw, got %+v", c2.Messages)
	}
}

// TestMigration041PreservesBackgroundEpisodes ensures existing summaries survive
// the migration from memory_summaries to memory_episodes.
func TestMigration041PreservesBackgroundEpisodes(t *testing.T) {
	db, _ := databasesAtVersion(t, config.SQLite, 33)
	ctx := context.Background()
	// Insert a background summary at version 33 (memory_summaries still exists).
	if err := db.orm.Exec("INSERT INTO chat_directory(agent_id,bot_id,chat_id,title,kind) VALUES ('test',1,100,'c','private') ON CONFLICT DO NOTHING").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.orm.Exec("INSERT INTO memory_summaries(agent_id,bot_id,chat_id,thread_id,through_id,content) VALUES ('test',1,100,0,42,'existing memory')").Error; err != nil {
		t.Fatal(err)
	}
	// Apply migration 041.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	// memory_summaries should be gone.
	var tableCount int64
	if err := db.orm.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='memory_summaries'").Scan(&tableCount).Error; err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatal("memory_summaries still exists after migration 041")
	}
	// The row should be in memory_episodes as a background episode.
	var rows []struct {
		Kind      string
		ThroughID int64
		Content   string
	}
	if err := db.orm.Table("memory_episodes").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != "background" || rows[0].ThroughID != 42 || rows[0].Content != "existing memory" {
		t.Fatalf("unexpected memory_episodes after migration: %+v", rows)
	}
}
