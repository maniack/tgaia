package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/maniack/tgaia/internal/config"
	"github.com/maniack/tgaia/internal/queue"
)

func queueDatabases(t *testing.T, driver config.Driver) (*Database, *Database) {
	return databasesAtVersion(t, driver, 17)
}
func databasesAtVersion(t *testing.T, driver config.Driver, version int) (*Database, *Database) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "queue.db")
	cfg := config.Config{Database: config.Database{Driver: driver}, ConnectTimeout: 5 * time.Second}
	if driver == config.Postgres {
		dsn = os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is required for PostgreSQL queue conformance")
		}
		cfg.Database.DSN = config.NewSecret(dsn)
		admin, err := Open(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		schema := "queue_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if err := admin.orm.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			admin.Close()
			t.Fatal("create test schema failed")
		}
		t.Cleanup(func() { admin.orm.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal("invalid test DSN")
		}
		values := u.Query()
		values.Set("search_path", schema)
		u.RawQuery = values.Encode()
		dsn = u.String()
	}
	cfg.Database.DSN = config.NewSecret(dsn)
	first, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { first.Close() })
	if err := first.migrate(context.Background(), version); err != nil {
		t.Fatal(err)
	}
	if err := first.migrate(context.Background(), version); err != nil {
		t.Fatal("migration is not idempotent")
	}
	if version >= 3 {
		registerTestBots(t, first)
	}
	second, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.Close() })
	return first, second
}
func enqueue(t *testing.T, q *Queue, chat int64, key string, max int) {
	t.Helper()
	applied, err := q.Enqueue(context.Background(), queue.Input{Scope: queue.Scope{AgentID: "test", BotID: 1, ChatID: chat}, Key: key, Kind: "test", Payload: []byte(`{"text":"private"}`), MaxAttempts: max})
	if err != nil || !applied {
		t.Fatalf("enqueue: applied=%v err=%v", applied, err)
	}
}
func claim(t *testing.T, q *Queue) *queue.Job {
	t.Helper()
	job, err := q.Claim(context.Background(), []string{"test"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}
	return job
}

func TestQueueConformance(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			t.Run("dedupe ordering fencing and recovery", func(t *testing.T) {
				db, other := queueDatabases(t, driver)
				q, peer := db.Queue(), other.Queue()
				ctx := context.Background()
				enqueue(t, q, 1, "a", 3)
				enqueue(t, q, 1, "b", 3)
				applied, err := peer.Enqueue(ctx, queue.Input{Scope: queue.Scope{AgentID: "test", BotID: 1, ChatID: 1}, Key: "a", Kind: "test", Payload: []byte(`{}`), MaxAttempts: 3})
				if err != nil || applied {
					t.Fatal("dedupe failed")
				}
				first := claim(t, q)
				if next, err := peer.Claim(ctx, []string{"test"}, time.Minute); err != nil || next != nil {
					t.Fatal("same scope ran concurrently")
				}
				enqueue(t, peer, 2, "a", 3)
				parallel := claim(t, peer)
				if parallel.Scope.ChatID != 2 {
					t.Fatal("other chat blocked")
				}
				if err := peer.Complete(ctx, parallel.Receipt); err != nil {
					t.Fatal(err)
				}
				if err := db.orm.Exec("UPDATE queue_jobs SET lease_until=0 WHERE id=?", first.ID).Error; err != nil {
					t.Fatal(err)
				}
				replacement := claim(t, peer)
				if replacement.ID != first.ID || replacement.Attempt != 2 || replacement.Token == first.Token {
					t.Fatal("recovery did not replace lease")
				}
				for _, operation := range []func() error{
					func() error { return q.Complete(ctx, first.Receipt) },
					func() error { return q.Retry(ctx, first.Receipt, time.Second) },
					func() error { return q.Extend(ctx, first.Receipt, time.Minute) },
				} {
					if !errors.Is(operation(), queue.ErrLeaseLost) {
						t.Fatal("stale owner changed state")
					}
				}
				if err := peer.Extend(ctx, replacement.Receipt, time.Minute); err != nil {
					t.Fatal(err)
				}
				if err := peer.Retry(ctx, replacement.Receipt, time.Minute); err != nil {
					t.Fatal(err)
				}
				if next, err := q.Claim(ctx, []string{"test"}, time.Minute); err != nil || next != nil {
					t.Fatal("retry delay/FIFO ignored")
				}
				db.orm.Exec("UPDATE queue_jobs SET available_at=0 WHERE id=?", first.ID)
				final := claim(t, q)
				if final.Attempt != 3 {
					t.Fatal("attempt counter incorrect")
				}
				if err := q.Retry(ctx, final.Receipt, time.Second); err != nil {
					t.Fatal(err)
				}
				next := claim(t, peer)
				if next.ID == first.ID {
					t.Fatal("attempt limit ignored")
				}
				if err := peer.Complete(ctx, next.Receipt); err != nil {
					t.Fatal(err)
				}
			})
			t.Run("crash at last attempt", func(t *testing.T) {
				db, other := queueDatabases(t, driver)
				q := db.Queue()
				enqueue(t, q, 1, "a", 1)
				enqueue(t, q, 1, "b", 1)
				first := claim(t, q)
				db.orm.Exec("UPDATE queue_jobs SET lease_until=0 WHERE id=?", first.ID)
				next := claim(t, other.Queue())
				if next.ID == first.ID {
					t.Fatal("exhausted crashed job not retired")
				}
			})
			t.Run("multiple consumers", func(t *testing.T) {
				db, other := queueDatabases(t, driver)
				q, peer := db.Queue(), other.Queue()
				ctx := context.Background()
				for i := 1; i <= 24; i++ {
					enqueue(t, q, int64(i), "same-key", 2)
				}
				var wg sync.WaitGroup
				var mu sync.Mutex
				seen := map[int64]bool{}
				for i := 0; i < 8; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						store := q
						if i%2 != 0 {
							store = peer
						}
						for {
							job, err := store.Claim(ctx, []string{"test"}, time.Minute)
							if err != nil {
								t.Error(err)
								return
							}
							if job == nil {
								return
							}
							mu.Lock()
							if seen[job.ID] {
								t.Error("duplicate active claim")
							}
							seen[job.ID] = true
							mu.Unlock()
							if err := store.Complete(ctx, job.Receipt); err != nil {
								t.Error(err)
								return
							}
						}
					}(i)
				}
				wg.Wait()
				if len(seen) != 24 {
					t.Fatalf("claimed %d jobs", len(seen))
				}
			})
			t.Run("concurrent enqueue within scope", func(t *testing.T) {
				db, other := queueDatabases(t, driver)
				stores := []*Queue{db.Queue(), other.Queue()}
				var wg sync.WaitGroup
				for i := 0; i < 12; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						_, err := stores[i%2].Enqueue(context.Background(), queue.Input{Scope: queue.Scope{AgentID: "test", BotID: 1, ChatID: 1}, ActorUserID: int64(i + 1), Key: fmt.Sprint(i), Kind: "test", Payload: []byte(`{}`), MaxAttempts: 1})
						if err != nil {
							t.Error(err)
						}
					}(i)
				}
				wg.Wait()
				var previous int64
				for i := 0; i < 12; i++ {
					job := claim(t, stores[i%2])
					if job.ID <= previous {
						t.Fatal("non-monotonic sequence")
					}
					previous = job.ID
					if err := stores[i%2].Complete(context.Background(), job.Receipt); err != nil {
						t.Fatal(err)
					}
				}
			})
		})
	}
}

func TestWorkerReplicas(t *testing.T) {
	for _, driver := range []config.Driver{config.SQLite, config.Postgres} {
		t.Run(string(driver), func(t *testing.T) {
			first, second := queueDatabases(t, driver)
			for chat := int64(1); chat <= 4; chat++ {
				for n := 0; n < 4; n++ {
					enqueue(t, first.Queue(), chat, fmt.Sprint(n), 3)
				}
			}
			var mu sync.Mutex
			active := map[int64]bool{}
			handler := func(ctx context.Context, job queue.Job) error {
				mu.Lock()
				if active[job.Scope.ChatID] {
					t.Error("overlapping handlers in same chat")
				}
				active[job.Scope.ChatID] = true
				mu.Unlock()
				defer func() { mu.Lock(); delete(active, job.Scope.ChatID); mu.Unlock() }()
				timer := time.NewTimer(5 * time.Millisecond)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					return nil
				}
			}
			opts := queue.DefaultOptions()
			opts.Concurrency = 3
			opts.PollInterval = 5 * time.Millisecond
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 2)
			for _, db := range []*Database{first, second} {
				worker, err := queue.NewWorker(db.Queue(), map[string]queue.Handler{"test": handler}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
				if err != nil {
					t.Fatal(err)
				}
				go func() { done <- worker.Run(ctx) }()
			}
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			complete := false
			for !complete {
				select {
				case <-deadline.C:
					cancel()
					t.Error("workers did not drain queue")
					complete = true
				case <-ticker.C:
					var count int64
					if err := first.orm.Table("queue_jobs").Where("status = ?", "completed").Count(&count).Error; err != nil {
						t.Error("count failed")
						cancel()
						complete = true
					}
					if count == 16 {
						complete = true
					}
				}
			}
			cancel()
			for i := 0; i < 2; i++ {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func registerTestBots(t *testing.T, db *Database) {
	t.Helper()
	for _, bot := range []struct {
		agent string
		id    int64
	}{{"test", 1}, {"agent", 2}, {"other", 3}} {
		if _, err := db.Bots().Register(context.Background(), bot.agent, bot.id, "TEST_TOKEN"); err != nil {
			t.Fatal(err)
		}
	}
}
