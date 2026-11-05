package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/maniack/tgaia/internal/access"
	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/queue"
	"gorm.io/gorm"
)

func validTask(task agent.Task) bool {
	return (chat.Scope{AgentID: task.AgentID, BotID: task.BotID, ChatID: task.ChatID, ThreadID: task.ThreadID}).Valid() && task.UserID > 0 && len(task.ID) > 0 && len(task.ID) < 128 && len(task.Prompt) > 0 && len(task.Prompt) <= 16000 && task.RunAt > 0 && (task.IntervalMS == 0 || task.IntervalMS >= 60000) && task.IntervalMS <= int64((365*24*time.Hour)/time.Millisecond)
}
func (a *AgentStore) PutTask(ctx context.Context, task agent.Task, expected int64, quotas ...int) error {
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialize creation with chat blocks and role changes.
		if tx.Dialector.Name() == "postgres" {
			var id int64
			if err := tx.Raw("SELECT chat_id FROM chat_directory WHERE agent_id=? AND bot_id=? AND chat_id=? FOR UPDATE", task.AgentID, task.BotID, task.ChatID).Scan(&id).Error; err != nil {
				return err
			}
		}
		if tx.Dialector.Name() == "postgres" {
			var id int64
			if err := tx.Raw("SELECT user_id FROM seen_users WHERE agent_id=? AND user_id=? FOR UPDATE", task.AgentID, task.UserID).Scan(&id).Error; err != nil {
				return err
			}
		}
		if task.Active && len(quotas) > 0 && quotas[0] > 0 {
			var count int64
			if err := tx.Table("scheduled_tasks").Where("agent_id=? AND user_id=? AND active=? AND id<>?", task.AgentID, task.UserID, true, task.ID).Count(&count).Error; err != nil {
				return err
			}
			if count >= int64(quotas[0]) {
				return ErrQuota
			}
		}
		return (&AgentStore{db: tx}).putTask(ctx, task, expected)
	})
}
func (a *AgentStore) putTask(ctx context.Context, task agent.Task, expected int64) error {
	if !validTask(task) {
		return errors.New("invalid task")
	}
	scope := chat.Scope{AgentID: task.AgentID, BotID: task.BotID, ChatID: task.ChatID, ThreadID: task.ThreadID}
	if task.Active {
		if err := a.CheckActions(ctx, scope, task.UserID); err != nil {
			return err
		}
	}
	if expected == 0 {
		result := a.db.WithContext(ctx).Exec(`INSERT INTO scheduled_tasks (id,agent_id,bot_id,chat_id,thread_id,user_id,prompt,run_at,interval_ms,active) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT (id) DO NOTHING`, task.ID, task.AgentID, task.BotID, task.ChatID, task.ThreadID, task.UserID, task.Prompt, task.RunAt, task.IntervalMS, task.Active)
		if result.Error != nil {
			return errors.New("creating task failed")
		}
		if result.RowsAffected == 0 {
			var existing agent.Task
			if err := a.db.WithContext(ctx).Table("scheduled_tasks").Where("id=?", task.ID).Take(&existing).Error; err != nil || existing.AgentID != task.AgentID || existing.BotID != task.BotID || existing.ThreadID != task.ThreadID || existing.ChatID != task.ChatID || existing.UserID != task.UserID || existing.Prompt != task.Prompt {
				return errors.New("task identity conflict")
			}
		}
		return nil
	}
	result := a.db.WithContext(ctx).Exec(`UPDATE scheduled_tasks SET prompt=?,run_at=?,interval_ms=?,active=?,version=version+1 WHERE id=? AND version=? AND agent_id=? AND bot_id=? AND chat_id=? AND thread_id=? AND user_id=?`, task.Prompt, task.RunAt, task.IntervalMS, task.Active, task.ID, expected, task.AgentID, task.BotID, task.ChatID, task.ThreadID, task.UserID)
	if result.Error != nil {
		return errors.New("updating task failed")
	}
	if result.RowsAffected != 1 {
		return errors.New("task version conflict")
	}
	return nil
}
func (a *AgentStore) DeleteTask(ctx context.Context, id string, version int64) error {
	result := a.db.WithContext(ctx).Exec("DELETE FROM scheduled_tasks WHERE id=? AND version=?", id, version)
	if result.Error != nil || result.RowsAffected != 1 {
		return errors.New("task delete conflict")
	}
	return nil
}

// CompactionPolicy bounds how often a chat is compacted. A compaction rewrites
// the whole cumulative memory, so it should run per batch, not per message.
type CompactionPolicy struct {
	MinMessages  int // Compact once this many messages have left the window.
	MaxUncompact int // ...or earlier, once this many messages are not yet summarized (0 = ignore).
}

// Tick coordinates scheduler replicas through a short DB transaction. LLM work
// never runs here. Each occurrence has a stable queue key and scope.
func (a *AgentStore) Tick(ctx context.Context, window time.Duration, policies ...CompactionPolicy) error {
	policy := CompactionPolicy{MinMessages: 1}
	if len(policies) > 0 {
		policy = policies[0]
	}
	if policy.MinMessages < 1 {
		policy.MinMessages = 1
	}
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("UPDATE scheduler_lock SET version=version+1 WHERE id=1").Error; err != nil {
			return err
		}
		q := (&Database{orm: tx}).Queue()
		var now int64
		if err := tx.Raw("SELECT " + q.now).Scan(&now).Error; err != nil {
			return err
		}
		var tasks []agent.Task
		if err := tx.Table("scheduled_tasks").Where("active=? AND run_at<=?", true, now).Order("run_at,id").Limit(64).Find(&tasks).Error; err != nil {
			return err
		}
		for _, task := range tasks {
			scope := chat.Scope{AgentID: task.AgentID, BotID: task.BotID, ChatID: task.ChatID, ThreadID: task.ThreadID}
			if err := (&AgentStore{db: tx}).CheckActions(ctx, scope, task.UserID); err == nil {
				payload, _ := json.Marshal(agent.JobPayload{Prompt: task.Prompt})
				if _, err := q.Enqueue(ctx, queue.Input{Scope: scope, ActorUserID: task.UserID, Key: fmt.Sprintf("task:%s:%d:%d", task.ID, task.Version, task.RunAt), Kind: "respond", Payload: payload, MaxAttempts: 3}); err != nil {
					return err
				}
			} else if errors.Is(err, access.ErrDenied) || errors.Is(err, access.ErrBlocked) || errors.Is(err, ErrChatBlocked) {
				task.Active = false
			} else {
				return err
			}
			if task.IntervalMS == 0 {
				task.Active = false
			} else {
				task.RunAt += (1 + (now-task.RunAt)/task.IntervalMS) * task.IntervalMS
			}
			if err := tx.Exec("UPDATE scheduled_tasks SET active=?,run_at=?,version=version+1 WHERE id=?", task.Active, task.RunAt, task.ID).Error; err != nil {
				return err
			}
		}
		var scopes []struct {
			AgentID        string
			BotID          int64
			ChatID         int64
			ThroughID      int64
			FromID         int64
			MemoryRevision int64
			Eligible       int64
			Pending        int64
		}
		// Compaction is chat-wide: messages of every topic feed one summary.
		// eligible = unsummarized messages older than the window; pending = all
		// unsummarized messages. A chat is compacted per batch, and never while a
		// compaction for it is already queued: overlapping ranges would each
		// rewrite the same memory.
		cutoff := now - window.Milliseconds()
		if err := tx.Raw(`SELECT m.agent_id,m.bot_id,m.chat_id,c.memory_revision,COALESCE(s.through_id,0) AS from_id,
 MAX(CASE WHEN m.sent_at<=? THEN m.id END) AS through_id,
 SUM(CASE WHEN m.sent_at<=? THEN 1 ELSE 0 END) AS eligible,COUNT(*) AS pending FROM conversation_messages m
 JOIN chat_directory c ON c.agent_id=m.agent_id AND c.bot_id=m.bot_id AND c.chat_id=m.chat_id
 LEFT JOIN memory_summaries s ON s.agent_id=m.agent_id AND s.bot_id=m.bot_id AND s.chat_id=m.chat_id
 WHERE c.blocked=? AND m.id>COALESCE(s.through_id,0)
 AND NOT EXISTS (SELECT 1 FROM queue_jobs j WHERE j.agent_id=m.agent_id AND j.bot_id=m.bot_id AND j.chat_id=m.chat_id AND j.kind='compact' AND j.status IN ('pending','retry','leased'))
 GROUP BY m.agent_id,m.bot_id,m.chat_id,s.through_id,c.memory_revision ORDER BY MIN(m.id) LIMIT 64`, cutoff, cutoff, false).Scan(&scopes).Error; err != nil {
			return err
		}
		for _, s := range scopes {
			if s.Eligible == 0 || s.ThroughID == 0 {
				continue
			}
			if int(s.Eligible) < policy.MinMessages && (policy.MaxUncompact == 0 || int(s.Pending) <= policy.MaxUncompact) {
				continue
			}
			payload, _ := json.Marshal(agent.JobPayload{ThroughID: s.ThroughID})
			if _, err := q.Enqueue(ctx, queue.Input{Scope: chat.Scope{AgentID: s.AgentID, BotID: s.BotID, ChatID: s.ChatID}, Key: fmt.Sprintf("compact:0:%d:%d:%d", s.MemoryRevision, s.FromID, s.ThroughID), Kind: "compact", Payload: payload, MaxAttempts: 3}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return errors.New("scheduler tick failed")
	}
	return nil
}

type Dashboard struct {
	Bots       []map[string]any     `json:"bots"`
	Chats      []agent.Conversation `json:"chats"`
	Users      []map[string]any     `json:"users"`
	Tasks      []agent.Task         `json:"tasks"`
	Deliveries []map[string]any     `json:"deliveries"`
	Queue      []map[string]any     `json:"queue"`
	Dead       []map[string]any     `json:"dead"`
}

type OperationalStatus struct {
	Bots              int64            `json:"bots"`
	Chats             int64            `json:"chats"`
	Users             int64            `json:"users"`
	Messages          int64            `json:"messages"`
	RememberedFacts   int64            `json:"remembered_facts"`
	ActiveTasks       int64            `json:"active_tasks"`
	UncertainDelivery int64            `json:"uncertain_deliveries"`
	UncertainTools    int64            `json:"uncertain_tools"`
	LastActivityAt    int64            `json:"last_activity_at"`
	Queue             []map[string]any `json:"queue"`
	RecentFailures    []map[string]any `json:"recent_failures"`
}

func (a *AgentStore) OperationalStatus(ctx context.Context) (OperationalStatus, error) {
	result := OperationalStatus{Queue: []map[string]any{}, RecentFailures: []map[string]any{}}
	counts := []struct {
		sql  string
		dest *int64
	}{
		{"SELECT COUNT(*) FROM telegram_bots WHERE bot_id>0", &result.Bots},
		{"SELECT COUNT(*) FROM chat_directory", &result.Chats},
		{"SELECT COUNT(*) FROM seen_users", &result.Users},
		{"SELECT COUNT(*) FROM conversation_messages", &result.Messages},
		{"SELECT COUNT(*) FROM explicit_memories", &result.RememberedFacts},
		{"SELECT COUNT(*) FROM scheduled_tasks WHERE active=TRUE", &result.ActiveTasks},
		{"SELECT COUNT(*) FROM outbound_deliveries WHERE state IN ('sending','uncertain')", &result.UncertainDelivery},
		{"SELECT COUNT(*) FROM tool_steps WHERE state IN ('started','uncertain') AND step LIKE 'call:%'", &result.UncertainTools},
		{"SELECT COALESCE(MAX(sent_at),0) FROM conversation_messages", &result.LastActivityAt},
	}
	for _, count := range counts {
		if err := a.db.WithContext(ctx).Raw(count.sql).Scan(count.dest).Error; err != nil {
			return result, errors.New("loading operational status failed")
		}
	}
	if err := a.db.WithContext(ctx).Raw("SELECT status,COUNT(*) AS count FROM queue_jobs GROUP BY status ORDER BY status").Scan(&result.Queue).Error; err != nil {
		return result, errors.New("loading operational status failed")
	}
	if err := a.db.WithContext(ctx).Raw("SELECT id,kind,agent_id,bot_id,chat_id,attempts,max_attempts,failure_class,updated_at FROM queue_jobs WHERE status='dead' ORDER BY updated_at DESC,id DESC LIMIT 10").Scan(&result.RecentFailures).Error; err != nil {
		return result, errors.New("loading operational status failed")
	}
	return result, nil
}

func (a *AgentStore) Dashboard(ctx context.Context) (Dashboard, error) {
	var result Dashboard
	queries := []struct {
		sql  string
		dest any
	}{
		{"SELECT agent_id,bot_id FROM telegram_bots WHERE bot_id>0 ORDER BY agent_id,bot_id LIMIT 200", &result.Bots},
		{"SELECT * FROM chat_directory ORDER BY agent_id,bot_id,chat_id LIMIT 200", &result.Chats},
		{"SELECT u.agent_id,u.user_id,u.username,u.name,u.language_code,u.is_premium,u.is_bot,u.role,u.version,CASE WHEN b.user_id IS NULL THEN FALSE ELSE TRUE END AS blocked FROM seen_users u LEFT JOIN blocked_users b ON u.agent_id=b.agent_id AND u.user_id=b.user_id ORDER BY u.agent_id,u.user_id LIMIT 200", &result.Users},
		{"SELECT * FROM scheduled_tasks ORDER BY run_at,id LIMIT 200", &result.Tasks},
		{"SELECT d.job_id,d.part,d.state,d.message_id,d.text,j.agent_id,j.bot_id,j.chat_id FROM outbound_deliveries d JOIN queue_jobs j ON j.id=d.job_id ORDER BY d.job_id DESC,d.part LIMIT 100", &result.Deliveries},
		{"SELECT status,COUNT(*) AS count FROM queue_jobs GROUP BY status", &result.Queue},
		{"SELECT id,agent_id,bot_id,chat_id,thread_id,kind,attempts,max_attempts,failure_class,updated_at FROM queue_jobs WHERE status='dead' ORDER BY id DESC LIMIT 50", &result.Dead},
	}
	for _, q := range queries {
		if err := a.db.WithContext(ctx).Raw(q.sql).Scan(q.dest).Error; err != nil {
			return result, errors.New("loading dashboard failed")
		}
	}
	return result, nil
}
func (a *AgentStore) BlockChat(ctx context.Context, s chat.Scope, blocked bool) error {
	if !s.Valid() {
		return errors.New("invalid chat")
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := scoped(tx.Table("chat_directory"), s).Update("blocked", blocked)
		if result.Error != nil || result.RowsAffected != 1 {
			return errors.New("chat update failed")
		}
		if blocked {
			if err := scoped(tx.Table("scheduled_tasks"), s).Where("active=?", true).Updates(map[string]any{"active": false, "version": gorm.Expr("version+1")}).Error; err != nil {
				return err
			}
			return scoped(tx.Table("queue_jobs"), s).Where("status IN ('pending','retry','leased')").Updates(map[string]any{"status": "dead", "lease_token": "", "lease_until": 0}).Error
		}
		return nil
	})
}
func (a *AgentStore) PutKnowledge(ctx context.Context, agentID, content string, version int64) error {
	if !validAgent(agentID) || len(content) > 16000 {
		return errors.New("invalid knowledge")
	}
	if version == 0 {
		r := a.db.WithContext(ctx).Exec("INSERT INTO shared_knowledge (agent_id,content) VALUES (?,?) ON CONFLICT DO NOTHING", agentID, content)
		if r.Error != nil || r.RowsAffected != 1 {
			return errors.New("knowledge conflict")
		}
		return nil
	}
	r := a.db.WithContext(ctx).Exec("UPDATE shared_knowledge SET content=?,version=version+1 WHERE agent_id=? AND version=?", content, agentID, version)
	if r.Error != nil || r.RowsAffected != 1 {
		return errors.New("knowledge conflict")
	}
	return nil
}
func (a *AgentStore) Knowledge(ctx context.Context, agentID string) (string, int64, error) {
	var row struct {
		Content string
		Version int64
	}
	err := a.db.WithContext(ctx).Table("shared_knowledge").Where("agent_id=?", agentID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, errors.New("reading knowledge failed")
	}
	return row.Content, row.Version, nil
}

// Tasks lists the scheduled tasks of one chat scope, soonest first. Tools show
// them to the model so it reports what is stored, not what it remembers saying.
func (a *AgentStore) Tasks(ctx context.Context, s chat.Scope) ([]agent.Task, error) {
	if !s.Valid() {
		return nil, errors.New("invalid task scope")
	}
	var tasks []agent.Task
	if err := scoped(a.db.WithContext(ctx).Table("scheduled_tasks"), s).Where("thread_id=?", s.ThreadID).Order("active DESC,run_at,id").Limit(100).Find(&tasks).Error; err != nil {
		return nil, errors.New("listing tasks failed")
	}
	return tasks, nil
}

// Task reads one task of a chat scope. Other chats' tasks are invisible even by ID.
func (a *AgentStore) Task(ctx context.Context, s chat.Scope, id string) (agent.Task, error) {
	var task agent.Task
	if !s.Valid() || id == "" {
		return task, errors.New("invalid task scope")
	}
	err := scoped(a.db.WithContext(ctx).Table("scheduled_tasks"), s).Where("thread_id=? AND id=?", s.ThreadID, id).Take(&task).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return task, ErrTaskNotFound
	}
	if err != nil {
		return task, errors.New("reading task failed")
	}
	return task, nil
}

var ErrTaskNotFound = errors.New("task not found")
