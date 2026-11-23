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
		args := []any{in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID, in.Scope.ThreadID, in.SenderChatID, in.ActorUserID, in.Key, in.Kind, string(in.Payload), in.MaxAttempts, in.Priority}
		if availAtArg != nil {
			args = append(args, availAtArg)
		}
		result := tx.Exec(`INSERT INTO queue_jobs (agent_id,bot_id,chat_id,thread_id,sender_chat_id,actor_user_id,dedupe_key,kind,payload,status,max_attempts,priority,available_at,created_at,updated_at)
 VALUES (?,?,?,?,?,?,?,?,?,'pending',?,?,`+availAt+`,`+q.now+`,`+q.now+`) ON CONFLICT (agent_id,bot_id,chat_id,dedupe_key) DO NOTHING`, args...)
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
// from the given sender in the same topic, if it has not been picked up yet.
// merge receives the job's current payload and returns the replacement, so a
// caller can anchor the reply to the latest message while keeping flags such
// as "addressed" from earlier ones. A job is bumped only within
// maxCoalesceSpans windows of its creation, so a sender posting continuously
// cannot postpone the reply forever. Returns true if a job was bumped.
const maxCoalesceSpans = 5

func (q *Queue) BumpAvailableAt(ctx context.Context, scope chat.Scope, actorUserID int64, newAvailAt int64, merge func(old []byte) []byte) (bool, error) {
	var row struct {
		ID      int64
		Payload string
	}
	found := q.db.WithContext(ctx).Raw(`SELECT id,payload FROM queue_jobs WHERE agent_id=? AND bot_id=? AND chat_id=? AND thread_id=? AND actor_user_id=?
           AND kind='respond' AND status='pending' AND available_at>=`+q.now+` AND created_at>=?
           ORDER BY id DESC LIMIT 1`, scope.AgentID, scope.BotID, scope.ChatID, scope.ThreadID, actorUserID, time.Now().UTC().Add(-maxCoalesceSpans*CoalescingDelay).UnixMilli()).Scan(&row)
	if found.Error != nil {
		return false, found.Error
	}
	if row.ID == 0 {
		return false, nil
	}
	r := q.db.WithContext(ctx).Exec(`UPDATE queue_jobs SET available_at=?,payload=?,updated_at=`+q.now+` WHERE id=? AND status='pending' AND available_at>=`+q.now, newAvailAt, string(merge([]byte(row.Payload))), row.ID)
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
 AND NOT EXISTS (SELECT 1 FROM queue_paused_scopes p WHERE p.agent_id=j.agent_id AND p.bot_id=j.bot_id AND p.chat_id=j.chat_id)
 AND ((j.status IN ('pending','retry') AND j.available_at<=` + q.now + `) OR (j.status='leased' AND j.lease_until<=` + q.now + `))
 AND NOT EXISTS (SELECT 1 FROM queue_jobs earlier WHERE earlier.agent_id=j.agent_id AND earlier.bot_id=j.bot_id AND earlier.chat_id=j.chat_id AND earlier.id<j.id AND earlier.priority<=j.priority AND earlier.status IN ('pending','leased','retry'))
 ORDER BY j.priority ASC, j.id LIMIT 1` + q.locking + `) RETURNING id,agent_id,bot_id,chat_id,thread_id,sender_chat_id,actor_user_id,kind,payload,attempts,max_attempts,lease_token`
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
var ErrNotCancellable = errors.New("job is not pending or retry")

// Cancel moves a pending or retry job to the cancelled status. Cancelled jobs
// are excluded from Replay and counted separately from failed ones.
func (q *Queue) Cancel(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("invalid job id")
	}
	result := q.db.WithContext(ctx).Exec(`UPDATE queue_jobs SET status='cancelled',lease_token='',lease_until=0,updated_at=`+q.now+` WHERE id=? AND status IN ('pending','retry')`, id)
	if result.Error != nil {
		return errors.New("queue cancel failed")
	}
	if result.RowsAffected != 1 {
		return ErrNotCancellable
	}
	return nil
}

// CancelScope cancels all pending/retry jobs in the given scope, optionally
// filtered by kind (empty = all kinds). Returns the number cancelled.
// When keepLast > 0, the keepLast most-recently-enqueued jobs of the given
// kind are preserved; older ones are cancelled.
func (q *Queue) CancelScope(ctx context.Context, agentID string, botID, chatID int64, kind string, keepLast int) (int64, error) {
	var kindFilter, keepFilter string
	var args []any
	args = append(args, agentID, botID, chatID)
	if kind != "" {
		kindFilter = " AND kind=?"
		args = append(args, kind)
	}
	if keepLast > 0 {
		keepFilter = " AND id NOT IN (SELECT id FROM queue_jobs WHERE agent_id=? AND bot_id=? AND chat_id=? AND status IN ('pending','retry')" + kindFilter + " ORDER BY id DESC LIMIT ?)"
		args = append(args, agentID, botID, chatID)
		if kind != "" {
			args = append(args, kind)
		}
		args = append(args, keepLast)
	}
	result := q.db.WithContext(ctx).Exec(`UPDATE queue_jobs SET status='cancelled',lease_token='',lease_until=0,updated_at=`+q.now+
		` WHERE agent_id=? AND bot_id=? AND chat_id=? AND status IN ('pending','retry')`+kindFilter+keepFilter, args...)
	if result.Error != nil {
		return 0, errors.New("queue cancel scope failed")
	}
	return result.RowsAffected, nil
}

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

// PauseScope prevents new jobs in the given scope from being claimed until ResumeScope is called.
// pausedBy is the user ID of the operator who paused it; 0 means system/automated.
func (q *Queue) PauseScope(ctx context.Context, agentID string, botID, chatID int64, pausedBy int64, reason string) error {
	if agentID == "" || chatID == 0 {
		return errors.New("invalid scope")
	}
	err := q.db.WithContext(ctx).Exec(`INSERT INTO queue_paused_scopes (agent_id,bot_id,chat_id,paused_at,paused_by,reason) VALUES (?,?,?,`+q.now+`,?,?)
 ON CONFLICT (agent_id,bot_id,chat_id) DO UPDATE SET paused_at=excluded.paused_at,paused_by=excluded.paused_by,reason=excluded.reason`, agentID, botID, chatID, pausedBy, reason).Error
	if err != nil {
		return errors.New("queue pause failed")
	}
	return nil
}

// ResumeScope re-enables job claiming for the given scope.
func (q *Queue) ResumeScope(ctx context.Context, agentID string, botID, chatID int64) error {
	if agentID == "" || chatID == 0 {
		return errors.New("invalid scope")
	}
	err := q.db.WithContext(ctx).Exec(`DELETE FROM queue_paused_scopes WHERE agent_id=? AND bot_id=? AND chat_id=?`, agentID, botID, chatID).Error
	if err != nil {
		return errors.New("queue resume failed")
	}
	return nil
}

// BacklogRow summarises the active queue depth for one scope.
type BacklogRow struct {
	AgentID        string `json:"agent_id"`
	BotID          int64  `json:"bot_id"`
	ChatID         int64  `json:"chat_id"`
	Pending        int64  `json:"pending"`
	Leased         int64  `json:"leased"`
	Retry          int64  `json:"retry"`
	OldestPendingMs int64 `json:"oldest_pending_ms"`
	ScopePaused    bool   `json:"scope_paused"`
}

// Backlog returns one row per scope that has pending, leased, or retry jobs.
func (q *Queue) Backlog(ctx context.Context) ([]BacklogRow, error) {
	var rows []BacklogRow
	err := q.db.WithContext(ctx).Raw(`
SELECT j.agent_id,j.bot_id,j.chat_id,
 SUM(CASE WHEN j.status='pending' THEN 1 ELSE 0 END) AS pending,
 SUM(CASE WHEN j.status='leased'  THEN 1 ELSE 0 END) AS leased,
 SUM(CASE WHEN j.status='retry'   THEN 1 ELSE 0 END) AS retry,
 MIN(CASE WHEN j.status='pending' THEN j.available_at END) AS oldest_pending_ms,
 EXISTS(SELECT 1 FROM queue_paused_scopes p WHERE p.agent_id=j.agent_id AND p.bot_id=j.bot_id AND p.chat_id=j.chat_id) AS scope_paused
FROM queue_jobs j
WHERE j.status IN ('pending','leased','retry')
GROUP BY j.agent_id,j.bot_id,j.chat_id
ORDER BY j.agent_id,j.bot_id,j.chat_id`).Scan(&rows).Error
	if err != nil {
		return nil, errors.New("queue backlog failed")
	}
	return rows, nil
}
