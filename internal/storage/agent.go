package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/maniack/tgaia/internal/access"
	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"github.com/maniack/tgaia/internal/messages"
	"github.com/maniack/tgaia/internal/queue"
	"gorm.io/gorm"
)

var ErrChatBlocked = errors.New("chat is blocked")

type AgentStore struct{ db *gorm.DB }

func (d *Database) Agent() *AgentStore { return &AgentStore{db: d.orm} }
func scoped(db *gorm.DB, s chat.Scope) *gorm.DB {
	return db.Where("agent_id=? AND bot_id=? AND chat_id=?", s.AgentID, s.BotID, s.ChatID)
}

// memoryScoped selects conversation memory for a chat. Memory (history,
// summaries, explicit facts) is shared by all forum topics of a chat since
// migration 018; only delivery destinations and tasks keep their topic.
func memoryScoped(db *gorm.DB, s chat.Scope) *gorm.DB {
	return scoped(db, s)
}
func (a *AgentStore) Ready(ctx context.Context) error {
	var count int64
	if err := a.db.WithContext(ctx).Table("schema_migrations").Where("version=18").Count(&count).Error; err != nil || count != 1 {
		return errors.New("schema is not ready; run db-migrate")
	}
	return nil
}
func (a *AgentStore) Check(ctx context.Context, s chat.Scope, userID int64) error {
	if !s.Valid() {
		return errors.New("invalid conversation scope")
	}
	var row agent.Conversation
	if err := scoped(a.db.WithContext(ctx).Table("chat_directory"), s).Take(&row).Error; err != nil {
		return errors.New("conversation not available")
	}
	if row.Blocked {
		return ErrChatBlocked
	}
	if userID > 0 {
		return (&Access{db: a.db}).CheckUser(ctx, s.AgentID, userID)
	}
	return nil
}
func (a *AgentStore) Accept(ctx context.Context, in agent.Incoming) error {
	if !in.Scope.Valid() || len(in.Attachments) > 10 || in.UpdateID < 0 || in.MessageID <= 0 || len(in.Text) > 64000 {
		return errors.New("invalid incoming message")
	}
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		inserted := tx.Exec("INSERT INTO telegram_inbox (bot_id,update_id) VALUES (?,?) ON CONFLICT DO NOTHING", in.Scope.BotID, in.UpdateID)
		if inserted.Error != nil {
			return inserted.Error
		}
		if inserted.RowsAffected == 0 {
			return nil
		}
		if err := tx.Exec(`INSERT INTO chat_directory (agent_id,bot_id,chat_id,title,kind) VALUES (?,?,?,?,?) ON CONFLICT (agent_id,bot_id,chat_id) DO UPDATE SET title=excluded.title,kind=excluded.kind`, in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID, in.Title, in.ChatType).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO chat_topics(agent_id,bot_id,chat_id,thread_id) VALUES (?,?,?,?) ON CONFLICT DO NOTHING", in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID, in.Scope.ThreadID).Error; err != nil {
			return err
		}
		if in.UserID > 0 {
			if err := tx.Exec(`INSERT INTO seen_users (agent_id,user_id,username,name) VALUES (?,?,?,?) ON CONFLICT (agent_id,user_id) DO UPDATE SET username=excluded.username,name=excluded.name`, in.Scope.AgentID, in.UserID, in.Username, in.Name).Error; err != nil {
				return err
			}
		}
		if err := (&AgentStore{db: tx}).Check(ctx, in.Scope, in.UserID); err != nil {
			if errors.Is(err, access.ErrBlocked) || errors.Is(err, ErrChatBlocked) {
				return nil
			}
			return err
		}
		attachments, err := json.Marshal(in.Attachments)
		if err != nil || len(attachments) > 32000 {
			return errors.New("invalid attachments")
		}
		var old agent.Message
		oldErr := scoped(tx.Table("conversation_messages"), in.Scope).Where("message_id=?", in.MessageID).Take(&old).Error
		if oldErr != nil && !errors.Is(oldErr, gorm.ErrRecordNotFound) {
			return oldErr
		}
		if oldErr == nil {
			if !in.Edited || old.UpdateID >= in.UpdateID {
				return nil
			}
			if old.ThreadID != in.Scope.ThreadID || (old.Role != "assistant" && (old.UserID != in.UserID || old.SenderChatID != in.SenderChatID)) {
				return errors.New("edited message identity conflict")
			}
			if old.Role == "assistant" {
				in.Respond = false
				in.ReplyMessageID = 0
			}
			if err := tx.Exec("UPDATE conversation_messages SET text=?,attachments=?,media_text='',update_id=? WHERE id=?", in.Text, string(attachments), in.UpdateID, old.ID).Error; err != nil {
				return err
			}
			if err := memoryScoped(tx.Table("memory_summaries"), in.Scope).Where("through_id>=?", old.ID).Delete(&struct{}{}).Error; err != nil {
				return err
			}
			if err := scoped(tx.Table("chat_directory"), in.Scope).Update("memory_revision", gorm.Expr("memory_revision+1")).Error; err != nil {
				return err
			}
			if err := memoryScoped(tx.Table("explicit_memories"), in.Scope).Where("source_message_id=?", in.MessageID).Delete(&struct{}{}).Error; err != nil {
				return err
			}
		} else {
			if err := tx.Exec(`INSERT INTO conversation_messages (agent_id,bot_id,chat_id,thread_id,message_id,user_id,sender_chat_id,update_id,attachments,role,text,sent_at) VALUES (?,?,?,?,?,?,?,?,?,'user',?,?)`, in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID, in.Scope.ThreadID, in.MessageID, in.UserID, in.SenderChatID, in.UpdateID, string(attachments), in.Text, in.SentAt.UnixMilli()).Error; err != nil {
				return err
			}
		}
		if !in.Respond && in.ReplyMessageID > 0 {
			var count int64
			if err := memoryScoped(tx.Table("bot_messages"), in.Scope).Where("message_id=? AND deleted_at IS NULL", in.ReplyMessageID).Count(&count).Error; err != nil {
				return err
			}
			in.Respond = count > 0
		}
		if !in.Respond || (in.UserID <= 0 && in.SenderChatID == 0) || in.Text == "" {
			return nil
		}
		payload, _ := json.Marshal(agent.JobPayload{MessageID: in.MessageID, SourceUpdateID: in.UpdateID, Edited: in.Edited})
		_, err = (&Database{orm: tx}).Queue().Enqueue(ctx, queue.Input{Scope: in.Scope, ActorUserID: in.UserID, SenderChatID: in.SenderChatID, Key: fmt.Sprintf("update:%d", in.UpdateID), Kind: "respond", Payload: payload, MaxAttempts: 3})
		return err
	})
	if err != nil {
		return errors.New("accepting Telegram update failed")
	}
	return nil
}
func (a *AgentStore) Context(ctx context.Context, s chat.Scope, through int64, limit int) (agent.Context, error) {
	var result agent.Context
	if !s.Valid() || limit < 1 || limit > 200 {
		return result, errors.New("invalid context query")
	}
	db := a.db.WithContext(ctx)
	err := memoryScoped(db.Table("memory_summaries"), s).Take(&result.Summary).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return result, errors.New("reading summary failed")
	}
	var knowledge struct{ Content string }
	err = db.Table("shared_knowledge").Where("agent_id=?", s.AgentID).Take(&knowledge).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return result, errors.New("reading knowledge failed")
	}
	result.Knowledge = knowledge.Content
	if err := memoryScoped(db.Table("explicit_memories"), s).Order("created_at DESC,source_message_id DESC").Limit(100).Find(&result.ExplicitMemories).Error; err != nil {
		return result, errors.New("reading explicit memory failed")
	}
	query := memoryScoped(db.Table("conversation_messages"), s).Where("id>?", result.Summary.ThroughID)
	if through > 0 {
		query = query.Where("id<=?", through)
	}
	order := "id DESC"
	if through > 0 {
		order = "id ASC"
	}
	if err := query.Order(order).Limit(limit).Find(&result.Messages).Error; err != nil {
		return result, errors.New("reading history failed")
	}
	if through == 0 {
		for l, r := 0, len(result.Messages)-1; l < r; l, r = l+1, r-1 {
			result.Messages[l], result.Messages[r] = result.Messages[r], result.Messages[l]
		}
	}
	return result, nil
}

func (a *AgentStore) ApplyExplicitMemory(ctx context.Context, job queue.Job, sourceMessageID, sourceUpdateID int64, content *string, embeddingModel string, embedding []float32) error {
	if sourceMessageID <= 0 || sourceUpdateID < 0 || job.ActorUserID <= 0 {
		return errors.New("invalid explicit memory source")
	}
	if content != nil && (strings.TrimSpace(*content) == "" || len(*content) > 2000) {
		return errors.New("invalid explicit memory")
	}
	encodedEmbedding, err := encodeEmbedding(embeddingModel, embedding)
	if err != nil {
		return err
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		store := &AgentStore{db: tx}
		if err := store.Check(ctx, job.Scope, job.ActorUserID); err != nil {
			return err
		}
		var count int64
		if err := memoryScoped(tx.Table("conversation_messages"), job.Scope).Where("message_id=? AND update_id=? AND user_id=? AND role='user'", sourceMessageID, sourceUpdateID, job.ActorUserID).Count(&count).Error; err != nil {
			return errors.New("checking explicit memory source failed")
		}
		if count != 1 {
			return nil
		}
		if content == nil {
			if err := memoryScoped(tx.Table("explicit_memories"), job.Scope).Where("source_message_id=? AND created_by=?", sourceMessageID, job.ActorUserID).Delete(&struct{}{}).Error; err != nil {
				return errors.New("deleting explicit memory failed")
			}
			return nil
		}
		if err := store.CheckActions(ctx, job.Scope, job.ActorUserID); err != nil {
			return err
		}
		value := strings.TrimSpace(*content)
		if err := tx.Exec(`INSERT INTO explicit_memories(agent_id,bot_id,chat_id,thread_id,source_message_id,source_update_id,created_by,content,embedding_model,embedding,created_at)
 VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(agent_id,bot_id,chat_id,thread_id,source_message_id)
 DO UPDATE SET source_update_id=excluded.source_update_id,created_by=excluded.created_by,content=excluded.content,embedding_model=excluded.embedding_model,embedding=excluded.embedding,created_at=excluded.created_at`, job.Scope.AgentID, job.Scope.BotID, job.Scope.ChatID, job.Scope.ThreadID, sourceMessageID, sourceUpdateID, job.ActorUserID, value, embeddingModel, encodedEmbedding, time.Now().UTC().UnixMilli()).Error; err != nil {
			return errors.New("saving explicit memory failed")
		}
		return nil
	})
}

func encodeEmbedding(model string, vector []float32) (string, error) {
	if model == "" && len(vector) == 0 {
		return "[]", nil
	}
	if model == "" || len(model) > 256 || len(vector) < 1 || len(vector) > 4096 {
		return "", errors.New("invalid explicit memory embedding")
	}
	norm := 0.0
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return "", errors.New("invalid explicit memory embedding")
		}
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return "", errors.New("invalid explicit memory embedding")
	}
	scale := float32(1 / math.Sqrt(norm))
	normalized := append([]float32(nil), vector...)
	for i := range normalized {
		normalized[i] *= scale
	}
	data, err := json.Marshal(normalized)
	if err != nil || len(data) > 128000 {
		return "", errors.New("invalid explicit memory embedding")
	}
	return string(data), nil
}

func (a *AgentStore) SearchExplicitMemories(ctx context.Context, scope chat.Scope, model string, query []float32, limit int) ([]agent.ExplicitMemory, error) {
	if !scope.Valid() || model == "" || limit < 1 || limit > 100 {
		return nil, errors.New("invalid explicit memory search")
	}
	encoded, err := encodeEmbedding(model, query)
	if err != nil {
		return nil, err
	}
	var normalized []float32
	if json.Unmarshal([]byte(encoded), &normalized) != nil {
		return nil, errors.New("invalid explicit memory search")
	}
	var rows []agent.ExplicitMemory
	if err := memoryScoped(a.db.WithContext(ctx).Table("explicit_memories"), scope).Where("embedding_model=?", model).Order("created_at DESC,source_message_id DESC").Limit(500).Find(&rows).Error; err != nil {
		return nil, errors.New("searching explicit memory failed")
	}
	type match struct {
		memory agent.ExplicitMemory
		score  float64
	}
	matches := make([]match, 0, len(rows))
	for _, row := range rows {
		var vector []float32
		if json.Unmarshal([]byte(row.Embedding), &vector) != nil || len(vector) != len(normalized) {
			continue
		}
		score := 0.0
		for i := range vector {
			score += float64(vector[i] * normalized[i])
		}
		matches = append(matches, match{memory: row, score: score})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score == matches[j].score {
			return matches[i].memory.CreatedAt > matches[j].memory.CreatedAt
		}
		return matches[i].score > matches[j].score
	})
	if len(matches) > limit {
		matches = matches[:limit]
	}
	result := make([]agent.ExplicitMemory, len(matches))
	for i := range matches {
		result[i] = matches[i].memory
	}
	return result, nil
}
func (a *AgentStore) SaveSummary(ctx context.Context, job queue.Job, previous, through int64, content string, revision ...int64) error {
	if len(content) == 0 || len(content) > 16000 || through <= previous {
		return errors.New("invalid summary")
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		if err := scoped(tx.Table("chat_directory"), job.Scope).Update("memory_revision", gorm.Expr("memory_revision")).Error; err != nil {
			return err
		}
		if len(revision) > 0 {
			current, err := (&AgentStore{db: tx}).MemoryRevision(ctx, job.Scope)
			if err != nil {
				return err
			}
			if current != revision[0] {
				return errors.New("memory changed during compaction")
			}
		}
		var existing struct{ ThroughID int64 }
		err := memoryScoped(tx.Table("memory_summaries"), job.Scope).Take(&existing).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return errors.New("summary read failed")
		}
		if existing.ThroughID >= through {
			return nil
		}
		if existing.ThroughID != previous {
			return errors.New("summary checkpoint conflict")
		}
		// One chat-wide summary row, kept in topic 0 regardless of the job's topic.
		if err := tx.Exec(`INSERT INTO memory_summaries (agent_id,bot_id,chat_id,thread_id,through_id,content) VALUES (?,?,?,0,?,?) ON CONFLICT (agent_id,bot_id,chat_id,thread_id) DO UPDATE SET through_id=excluded.through_id,content=excluded.content`, job.Scope.AgentID, job.Scope.BotID, job.Scope.ChatID, through, content).Error; err != nil {
			return errors.New("saving summary failed")
		}
		return nil
	})
}
func fence(tx *gorm.DB, job queue.Job) error {
	return (&Database{orm: tx}).Queue().Extend(tx.Statement.Context, job.Receipt, 30*time.Second)
}
func (a *AgentStore) Result(ctx context.Context, id int64) (string, error) {
	var row struct{ Result string }
	err := a.db.WithContext(ctx).Table("job_results").Where("job_id=?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("reading job result failed")
	}
	return row.Result, nil
}
func (a *AgentStore) SaveResult(ctx context.Context, job queue.Job, result string) error {
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO job_results (job_id,result) VALUES (?,?) ON CONFLICT DO NOTHING", job.ID, result).Error; err != nil {
			return errors.New("saving job result failed")
		}
		return nil
	})
}
func (a *AgentStore) BeginSend(ctx context.Context, job queue.Job, part int, text string) (bool, error) {
	applied := false
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		if err := (&AgentStore{db: tx}).Check(ctx, job.Scope, job.ActorUserID); err != nil {
			return err
		}
		r := tx.Exec("INSERT INTO outbound_deliveries (job_id,part,state,text) VALUES (?,?,'sending',?) ON CONFLICT DO NOTHING", job.ID, part, text)
		applied = r.RowsAffected == 1
		return r.Error
	})
	if err != nil {
		return false, errors.New("preparing delivery failed")
	}
	return applied, nil
}

// AbandonSend withdraws a delivery intent that Telegram definitely rejected
// before anything was sent, so the same part can be delivered in another form.
func (a *AgentStore) AbandonSend(ctx context.Context, job queue.Job, part int) error {
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		return tx.Exec("DELETE FROM outbound_deliveries WHERE job_id=? AND part=? AND state='sending'", job.ID, part).Error
	})
	if err != nil {
		return errors.New("withdrawing delivery failed")
	}
	return nil
}
func (a *AgentStore) MarkUncertain(ctx context.Context, id int64, part int) error {
	if err := a.db.WithContext(ctx).Exec("UPDATE outbound_deliveries SET state='uncertain',version=version+1 WHERE job_id=? AND part=? AND state='sending'", id, part).Error; err != nil {
		return errors.New("marking uncertain delivery failed")
	}
	return nil
}
func (a *AgentStore) Sent(ctx context.Context, job queue.Job, part int, text string, messageID int64, at time.Time) error {
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		outcome, err := (&BotMessages{db: tx}).RecordSent(ctx, messages.Sent{Reference: messages.Reference{Scope: job.Scope, MessageID: messageID}, DeliveryKey: fmt.Sprintf("job:%d", job.ID), Part: part, SentAt: at})
		if err != nil {
			return err
		}
		if outcome == messages.Conflict {
			return errors.New("conflicting send receipt")
		}
		if err := tx.Exec(`INSERT INTO conversation_messages (agent_id,bot_id,chat_id,thread_id,message_id,user_id,role,text,sent_at) VALUES (?,?,?,?,?,0,'assistant',?,?) ON CONFLICT DO NOTHING`, job.Scope.AgentID, job.Scope.BotID, job.Scope.ChatID, job.Scope.ThreadID, messageID, text, at.UnixMilli()).Error; err != nil {
			return err
		}
		if err := (&AgentStore{db: tx}).linkReply(ctx, job, part, messageID); err != nil {
			return err
		}
		return tx.Exec("UPDATE outbound_deliveries SET state='sent',version=version+1,message_id=? WHERE job_id=? AND part=?", messageID, job.ID, part).Error
	})
	if err != nil {
		return errors.New("saving delivery receipt failed")
	}
	return nil
}
func (a *AgentStore) DeliveryState(ctx context.Context, id int64, part int) (string, error) {
	var row struct{ State string }
	if err := a.db.WithContext(ctx).Table("outbound_deliveries").Where("job_id=? AND part=?", id, part).Take(&row).Error; err != nil {
		return "", errors.New("delivery state unavailable")
	}
	if row.State == "uncertain" {
		var count int64
		if err := a.db.WithContext(ctx).Table("outcome_resolutions").Where("job_id=? AND target=? AND action='dismiss'", id, fmt.Sprintf("delivery:%d", part)).Count(&count).Error; err != nil {
			return "", errors.New("reading resolution failed")
		}
		if count > 0 {
			return "dismissed", nil
		}
	}
	return row.State, nil
}
func (a *AgentStore) ReplyContext(ctx context.Context, s chat.Scope, messageID int64, limits ...int) (agent.Context, time.Time, error) {
	limit := 30
	if len(limits) > 0 {
		limit = limits[0]
	}
	if limit < 1 || limit > 200 {
		return agent.Context{}, time.Time{}, errors.New("invalid reply context limit")
	}
	if messageID == 0 {
		c, err := a.Context(ctx, s, 0, 30)
		return c, time.Now(), err
	}
	if !s.Valid() {
		return agent.Context{}, time.Time{}, errors.New("invalid reply scope")
	}
	var target agent.Message
	if err := memoryScoped(a.db.WithContext(ctx).Table("conversation_messages"), s).Where("message_id=? AND role='user'", messageID).Take(&target).Error; err != nil {
		return agent.Context{}, time.Time{}, errors.New("reply target missing")
	}
	c, err := a.Context(ctx, s, 0, 1)
	if err != nil {
		return c, time.Time{}, err
	}
	if c.Summary.ThroughID >= target.ID {
		c.Summary = agent.Summary{}
	}
	c.Messages = nil
	if err := memoryScoped(a.db.WithContext(ctx).Table("conversation_messages"), s).Where("id>? AND id<=?", c.Summary.ThroughID, target.ID).Order("id DESC").Limit(limit).Find(&c.Messages).Error; err != nil {
		return c, time.Time{}, errors.New("reply context unavailable")
	}
	for l, r := 0, len(c.Messages)-1; l < r; l, r = l+1, r-1 {
		c.Messages[l], c.Messages[r] = c.Messages[r], c.Messages[l]
	}
	return c, time.UnixMilli(target.SentAt), nil
}
func (a *AgentStore) BindEnvironment(ctx context.Context, mock bool) error {
	family := "live"
	if mock {
		family = "mock"
	}
	if err := a.db.WithContext(ctx).Exec("INSERT INTO runtime_identity (id,telegram_family) VALUES (1,?) ON CONFLICT DO NOTHING", family).Error; err != nil {
		return errors.New("binding runtime environment failed")
	}
	var row struct{ TelegramFamily string }
	if err := a.db.WithContext(ctx).Table("runtime_identity").Where("id=1").Take(&row).Error; err != nil {
		return errors.New("reading runtime environment failed")
	}
	if row.TelegramFamily != family {
		return errors.New("mock and live Telegram must use separate databases")
	}
	return nil
}
func (a *AgentStore) SyncMessage(ctx context.Context, s chat.Scope, id int64, text string, deleted bool) error {
	if !s.Valid() || id <= 0 {
		return errors.New("invalid message scope")
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row agent.Message
		err := memoryScoped(tx.Table("conversation_messages"), s).Where("message_id=? AND role='assistant'", id).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return errors.New("reading edited history failed")
		}
		if deleted {
			text = "[Bot message deleted]"
		}
		if row.Text == text {
			return nil
		}
		if err := memoryScoped(tx.Table("conversation_messages"), s).Where("id=?", row.ID).Update("text", text).Error; err != nil {
			return errors.New("updating history failed")
		}
		if err := memoryScoped(tx.Table("memory_summaries"), s).Where("through_id>=?", row.ID).Delete(&struct{}{}).Error; err != nil {
			return errors.New("invalidating summary failed")
		}
		if err := scoped(tx.Table("chat_directory"), s).Update("memory_revision", gorm.Expr("memory_revision+1")).Error; err != nil {
			return errors.New("advancing memory revision failed")
		}
		return nil
	})
}

func (a *AgentStore) MemoryRevision(ctx context.Context, s chat.Scope) (int64, error) {
	var row struct{ MemoryRevision int64 }
	err := scoped(a.db.WithContext(ctx).Table("chat_directory"), s).Take(&row).Error
	return row.MemoryRevision, err
}
func (a *AgentStore) ResponseCurrent(ctx context.Context, s chat.Scope, id, update int64) (bool, error) {
	if update == 0 {
		return true, nil
	}
	var count int64
	err := memoryScoped(a.db.WithContext(ctx).Table("conversation_messages"), s).Where("message_id=? AND update_id=?", id, update).Count(&count).Error
	return count == 1, err
}
func (a *AgentStore) SaveMediaText(ctx context.Context, job queue.Job, id, update int64, text string) error {
	if len(text) > 32000 {
		return errors.New("attachment text too large")
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		r := memoryScoped(tx.Table("conversation_messages"), job.Scope).Where("id=? AND update_id=? AND media_text=''", id, update).Update("media_text", text)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return errors.New("attachment revision changed")
		}
		return nil
	})
}
