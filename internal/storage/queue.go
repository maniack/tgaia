package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/access"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/queue"
	"gorm.io/gorm"
)

// Queue uses database time for leases, independent of pod clock skew.
type Queue struct {
	db      *gorm.DB
	now     string
	locking string
}

func (d *Database) Queue() *Queue {
	q := &Queue{db: d.orm, now: "CAST((julianday('now') - 2440587.5) * 86400000 AS BIGINT)"}
	if d.orm.Dialector.Name() == "postgres" {
		q.now = "CAST(EXTRACT(EPOCH FROM clock_timestamp()) * 1000 AS BIGINT)"
		q.locking = " FOR UPDATE SKIP LOCKED"
	}
	return q
}

var _ queue.Store = (*Queue)(nil)

// Enqueue is idempotent. An existing key is unchanged, including completed/dead
// jobs: retry producers must not replace the original payload or revive work.
func (q *Queue) Enqueue(ctx context.Context, in queue.Input) (bool, error) {
	if !in.Scope.Valid() || in.ActorUserID < 0 || strings.TrimSpace(in.Key) == "" || len(in.Key) > 256 || strings.TrimSpace(in.Kind) == "" || len(in.Kind) > 64 || in.MaxAttempts < 1 || in.MaxAttempts > 100 || len(in.Payload) > 1<<20 || !json.Valid(in.Payload) {
		return false, errors.New("invalid queue job")
	}
	var applied bool
	err := q.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialize enqueues within a scope before allocating the sequence ID.
		// Otherwise an uncommitted lower ID could appear after a later job was claimed.
		if err := tx.Exec(`INSERT INTO queue_scopes (agent_id,bot_id,chat_id) VALUES (?,?,?)
   ON CONFLICT (agent_id,bot_id,chat_id) DO UPDATE SET agent_id=excluded.agent_id`, in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID).Error; err != nil {
			return err
		}
		if in.ActorUserID > 0 {
			if err := (&Access{db: tx}).CheckUser(ctx, in.Scope.AgentID, in.ActorUserID); err != nil {
				return err
			}
		}
		availAt := q.now
		var availAtArg any
		if in.AvailableAt > 0 {
			availAt = "?"
			availAtArg = in.AvailableAt
		}
		args := []any{in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID, in.Scope.ThreadID, in.SenderChatID, in.ActorUserID, in.Key, in.Kind, string(in.Payload), in.MaxAttempts}
		if availAtArg != nil {
			args = append(args, availAtArg)
		}
		result := tx.Exec(`INSERT INTO queue_jobs (agent_id,bot_id,chat_id,thread_id,sender_chat_id,actor_user_id,dedupe_key,kind,payload,status,max_attempts,available_at,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,?,?,'pending',?,`+availAt+`,`+q.now+`,`+q.now+`) ON CONFLICT (agent_id,bot_id,chat_id,dedupe_key) DO NOTHING`, args...)
		applied = result.RowsAffected == 1
		return result.Error
	})
	if errors.Is(err, access.ErrBlocked) {
		return false, access.ErrBlocked
	}
	if err != nil {
		return false, errors.New("queue enqueue failed")
	}
	return applied, nil
}

// BumpAvailableAt extends available_at of the most recent pending respond job
// from the given user in scope, if one exists and its current available_at is
// before the given deadline. Returns true if a row was updated.
func (q *Queue) BumpAvailableAt(ctx context.Context, scope chat.Scope, actorUserID int64, newAvailAt int64) (bool, error) {
	r := q.db.WithContext(ctx).Exec(`UPDATE queue_jobs SET available_at=?,updated_at=`+q.now+`
 WHERE id=(SELECT id FROM queue_jobs WHERE agent_id=? AND bot_id=? AND chat_id=? AND actor_user_id=?
           AND kind='respond' AND status='pending' AND available_at>=`+q.now+`
           ORDER BY id DESC LIMIT 1)
 AND available_at<?`, newAvailAt, scope.AgentID, scope.BotID, scope.ChatID, actorUserID, newAvailAt)
	if r.Error != nil {
		return false, r.Error
	}
	return r.RowsAffected == 1, nil
}

type claimedJob struct {
	ID           int64
	AgentID      string
	BotID        int64
	ThreadID     int64
	SenderChatID int64
	ActorUserID  int64
	ChatID       int64
	Kind         string
	Payload      string
	Attempts     int
	MaxAttempts  int
	LeaseToken   string
}

func validDuration(d time.Duration) bool { return d >= time.Millisecond && d <= 24*time.Hour }

func (q *Queue) Claim(ctx context.Context, kinds []string, lease time.Duration) (*queue.Job, error) {
	if len(kinds) == 0 || !validDuration(lease) {
		return nil, errors.New("invalid queue claim settings")
	}
	// Suppress queued work from blocked users, including scheduled work. Clear
	// live receipts too; the next heartbeat cancels the old handler. The external
	// adapter must independently re-check access immediately before side effects.
	blocked := q.db.WithContext(ctx).Exec(`UPDATE queue_jobs SET status='dead',lease_token='',lease_until=0,updated_at=` + q.now + `
 WHERE id IN (SELECT id FROM queue_jobs WHERE status IN ('pending','retry','leased') AND actor_user_id>0
 AND EXISTS (SELECT 1 FROM blocked_users b WHERE b.agent_id=queue_jobs.agent_id AND b.user_id=queue_jobs.actor_user_id)
 ORDER BY id LIMIT 64` + q.locking + `)`)
	if blocked.Error != nil {
		return nil, errors.New("queue access sweep failed")
	}
	// Bound recovery work per poll. Exhausted crashed jobs must not block a chat.
	sweep := q.db.WithContext(ctx).Exec(`UPDATE queue_jobs SET status='dead',lease_token='',lease_until=0,updated_at=` + q.now + `
 WHERE id IN (SELECT id FROM queue_jobs WHERE status='leased' AND lease_until<=` + q.now + ` AND attempts>=max_attempts ORDER BY id LIMIT 64` + q.locking + `)`)
	if sweep.Error != nil {
		return nil, errors.New("queue recovery failed")
	}
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, errors.New("queue token generation failed")
	}
	token := hex.EncodeToString(tokenBytes)
	// Only the earliest nonterminal job in a scope is claimable. SKIP LOCKED
	// cannot leapfrog a locked head, because that head still blocks later rows.
	query := `UPDATE queue_jobs SET status='leased',attempts=attempts+1,lease_token=?,lease_until=` + q.now + `+?,updated_at=` + q.now + `
 WHERE id=(SELECT j.id FROM queue_jobs j
 WHERE j.bot_id>0 AND j.kind IN ?
 AND NOT EXISTS (SELECT 1 FROM blocked_users b WHERE b.agent_id=j.agent_id AND b.user_id=j.actor_user_id) AND j.attempts<j.max_attempts
 AND ((j.status IN ('pending','retry') AND j.available_at<=` + q.now + `) OR (j.status='leased' AND j.lease_until<=` + q.now + `))
 AND NOT EXISTS (SELECT 1 FROM queue_jobs earlier WHERE earlier.agent_id=j.agent_id AND earlier.bot_id=j.bot_id AND earlier.chat_id=j.chat_id AND earlier.id<j.id AND earlier.status IN ('pending','leased','retry'))
 ORDER BY j.id LIMIT 1` + q.locking + `) RETURNING id,agent_id,bot_id,chat_id,thread_id,sender_chat_id,actor_user_id,kind,payload,attempts,max_attempts,lease_token`
	var row claimedJob
	result := q.db.WithContext(ctx).Raw(query, token, lease.Milliseconds(), kinds).Scan(&row)
	if result.Error != nil {
		return nil, errors.New("queue claim failed")
	}
	if result.RowsAffected == 0 {
		return nil, nil
	}
	return &queue.Job{Receipt: queue.Receipt{ID: row.ID, Token: row.LeaseToken}, Scope: queue.Scope{AgentID: row.AgentID, BotID: row.BotID, ChatID: row.ChatID, ThreadID: row.ThreadID}, SenderChatID: row.SenderChatID, ActorUserID: row.ActorUserID, Kind: row.Kind, Payload: []byte(row.Payload), Attempt: row.Attempts, MaxAttempts: row.MaxAttempts}, nil
}
func (q *Queue) owned(ctx context.Context, r queue.Receipt, assignments string, args ...any) error {
	args = append(args, r.ID, r.Token)
	result := q.db.WithContext(ctx).Exec(`UPDATE queue_jobs SET `+assignments+`,updated_at=`+q.now+` WHERE id=? AND lease_token=? AND status='leased' AND lease_until>`+q.now+`
 AND NOT EXISTS (SELECT 1 FROM blocked_users b WHERE b.agent_id=queue_jobs.agent_id AND b.user_id=queue_jobs.actor_user_id)`, args...)
	if result.Error != nil {
		return errors.New("queue lease update failed")
	}
	if result.RowsAffected != 1 {
		return queue.ErrLeaseLost
	}
	return nil
}
func (q *Queue) Extend(ctx context.Context, r queue.Receipt, lease time.Duration) error {
	if !validDuration(lease) {
		return errors.New("invalid lease duration")
	}
	return q.owned(ctx, r, "lease_until="+q.now+"+?", lease.Milliseconds())
}
func (q *Queue) Complete(ctx context.Context, r queue.Receipt) error {
	return q.owned(ctx, r, "status='completed',lease_token='',lease_until=0")
}
func (q *Queue) Retry(ctx context.Context, r queue.Receipt, delay time.Duration, class string) error {
	if !validDuration(delay) {
		return errors.New("invalid retry delay")
	}
	return q.owned(ctx, r, "status=CASE WHEN attempts>=max_attempts THEN 'dead' ELSE 'retry' END,available_at="+q.now+"+?,failure_class=?,lease_token='',lease_until=0", delay.Milliseconds(), class)
}

// Fail retires a job after a deterministic failure; the remaining attempts would
// only repeat the same work against the same input.
func (q *Queue) Fail(ctx context.Context, r queue.Receipt, class string) error {
	return q.owned(ctx, r, "status='dead',failure_class=?,lease_token='',lease_until=0", class)
}

var ErrNotDead = errors.New("job is not dead")

// Replay returns a dead job to the queue with a fresh attempt budget. Durable
// checkpoints (tool turns, cached results, delivery receipts) are preserved, so
// the replay resumes rather than repeats completed steps. The job runs in its
// original scope order like any other pending job.
func (q *Queue) Replay(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("invalid job")
	}
	result := q.db.WithContext(ctx).Exec(`UPDATE queue_jobs SET status='pending',attempts=0,failure_class='',lease_token='',lease_until=0,available_at=`+q.now+`,updated_at=`+q.now+`
 WHERE id=? AND status='dead' AND NOT EXISTS (SELECT 1 FROM blocked_users b WHERE b.agent_id=queue_jobs.agent_id AND b.user_id=queue_jobs.actor_user_id)`, id)
	if result.Error != nil {
		return errors.New("queue replay failed")
	}
	if result.RowsAffected != 1 {
		return ErrNotDead
	}
	return nil
}
