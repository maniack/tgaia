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

// CoalescingDelay is the window during which a second message from the same
// sender (human or bot) bumps an existing pending respond job instead of creating a new
// one. Tests may set it to zero to disable coalescing.
var CoalescingDelay = 3 * time.Second

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

// historyWithUsers reads conversation_messages joined with the sender's last
// seen name and username, so prompts can attribute every line to a person.
func historyWithUsers(db *gorm.DB, s chat.Scope) *gorm.DB {
	return db.Table("conversation_messages AS m").Select("m.*, u.username, u.name, COALESCE(u.is_bot,FALSE) AS is_bot").
		Joins("LEFT JOIN seen_users u ON u.agent_id=m.agent_id AND u.user_id=m.user_id").
		Where("m.agent_id=? AND m.bot_id=? AND m.chat_id=?", s.AgentID, s.BotID, s.ChatID)
}

// memoriesWithUsers reads explicit_memories joined with their author's identity.
func memoriesWithUsers(db *gorm.DB, s chat.Scope) *gorm.DB {
	return db.Table("explicit_memories AS e").Select("e.*, u.username AS created_by_username, u.name AS created_by_name").
		Joins("LEFT JOIN seen_users u ON u.agent_id=e.agent_id AND u.user_id=e.created_by").
		Where("e.agent_id=? AND e.bot_id=? AND e.chat_id=?", s.AgentID, s.BotID, s.ChatID)
}
func (a *AgentStore) Ready(ctx context.Context) error {
	var count int64
	if err := a.db.WithContext(ctx).Table("schema_migrations").Where("version=32").Count(&count).Error; err != nil || count != 1 {
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
	if in.IsBot && in.UserID == in.Scope.BotID {
		return nil
	}
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		inserted := tx.Exec("INSERT INTO telegram_inbox (bot_id,update_id) VALUES (?,?) ON CONFLICT DO NOTHING", in.Scope.BotID, in.UpdateID)
		if inserted.Error != nil {
			return inserted.Error
		}
		if inserted.RowsAffected == 0 {
			return nil
		}
		if err := tx.Exec(`INSERT INTO chat_directory (agent_id,bot_id,chat_id,title,kind,is_forum) VALUES (?,?,?,?,?,?) ON CONFLICT (agent_id,bot_id,chat_id) DO UPDATE SET title=excluded.title,kind=excluded.kind,is_forum=excluded.is_forum`, in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID, in.Title, in.ChatType, in.IsForum).Error; err != nil {
			return err
		}
		if err := tx.Exec("INSERT INTO chat_topics(agent_id,bot_id,chat_id,thread_id) VALUES (?,?,?,?) ON CONFLICT DO NOTHING", in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID, in.Scope.ThreadID).Error; err != nil {
			return err
		}
		if in.UserID > 0 {
			role := "user"
			if in.IsBot {
				role = "reader"
			}
			if err := tx.Exec(`INSERT INTO seen_users (agent_id,user_id,username,name,language_code,is_premium,is_bot,role) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (agent_id,user_id) DO UPDATE SET username=excluded.username,name=excluded.name,language_code=excluded.language_code,is_premium=excluded.is_premium,is_bot=excluded.is_bot`, in.Scope.AgentID, in.UserID, in.Username, in.Name, in.LanguageCode, in.IsPremium, in.IsBot, role).Error; err != nil {
				return err
			}
		}
		behavior, err := (&AgentStore{db: tx}).ChatBehavior(ctx, in.Scope)
		if err != nil {
			return err
		}
		addressed := in.Respond
		if (in.ChatType == "group" || in.ChatType == "supergroup") && behavior.GroupResponse == "all" {
			in.Respond = true
		}
		if in.IsBot && !behavior.RespondToBots {
			in.Respond = false
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
			if old.Text == in.Text && old.Attachments == string(attachments) {
				// Telegram re-sends edited_message for link-preview and similar
				// changes. Nothing the model sees changed: skip without touching
				// update_id so any in-flight respond job is not invalidated.
				return nil
			}
			if err := tx.Exec("UPDATE conversation_messages SET text=?,attachments=?,media_text='',update_id=?,reply_to_message_id=? WHERE id=?", in.Text, string(attachments), in.UpdateID, in.ReplyMessageID, old.ID).Error; err != nil {
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
			if err := tx.Exec(`INSERT INTO conversation_messages (agent_id,bot_id,chat_id,thread_id,message_id,user_id,sender_chat_id,update_id,attachments,role,text,sent_at,reply_to_message_id) VALUES (?,?,?,?,?,?,?,?,?,'user',?,?,?)`, in.Scope.AgentID, in.Scope.BotID, in.Scope.ChatID, in.Scope.ThreadID, in.MessageID, in.UserID, in.SenderChatID, in.UpdateID, string(attachments), in.Text, in.SentAt.UnixMilli(), in.ReplyMessageID).Error; err != nil {
				return err
			}
			// A picture posted without addressing the bot is still described, so
			// the chat memory knows what was shown. The job runs in chat order.
			if !in.Respond && hasImage(in.Attachments) {
				payload, _ := json.Marshal(agent.JobPayload{MessageID: in.MessageID, SourceUpdateID: in.UpdateID, Action: "describe"})
				if _, err := (&Database{orm: tx}).Queue().Enqueue(ctx, queue.Input{Scope: in.Scope, ActorUserID: in.UserID, SenderChatID: in.SenderChatID, Key: fmt.Sprintf("media:%d", in.UpdateID), Kind: "media", Payload: payload, MaxAttempts: 2}); err != nil {
					return err
				}
			}
		}
		if !in.Respond && in.ReplyMessageID > 0 {
			var count int64
			if err := memoryScoped(tx.Table("bot_messages"), in.Scope).Where("message_id=? AND deleted_at IS NULL", in.ReplyMessageID).Count(&count).Error; err != nil {
				return err
			}
			in.Respond = count > 0
			addressed = in.Respond
		}
		if old.Role == "assistant" {
			addressed = false
		}
		if in.IsBot && !behavior.RespondToBots {
			in.Respond = false
		}
		if in.IsBot && in.Respond && behavior.LoopProtection {
			// Bound autonomous bot-to-bot exchanges without affecting human
			// traffic. Disabling the per-bot toggle removes this special limit;
			// ordinary queue idempotency and global quotas still apply.
			// Counted over respond jobs, i.e. replies: messages coalesced into
			// one job produce one reply and count once.
			var recent int64
			cutoff := time.Now().UTC().Add(-5 * time.Minute).UnixMilli()
			if err := scoped(tx.Table("queue_jobs"), in.Scope).Where("kind='respond' AND actor_user_id=? AND created_at>=?", in.UserID, cutoff).Count(&recent).Error; err != nil {
				return err
			}
			if recent >= 6 {
				in.Respond = false
			}
		}
		if !in.Respond || (in.UserID <= 0 && in.SenderChatID == 0) || in.Text == "" {
			return nil
		}
		payload, _ := json.Marshal(agent.JobPayload{MessageID: in.MessageID, SourceUpdateID: in.UpdateID, Edited: in.Edited, Addressed: addressed})
		q := (&Database{orm: tx}).Queue()
		// Coalesce rapid messages from the same sender (human or bot): if a
		// pending respond job already exists, push its available_at forward
		// instead of creating a second job. The earlier message is already in
		// conversation_messages and will be part of the reply context either way.
		if CoalescingDelay > 0 {
			delay := time.Now().UTC().Add(CoalescingDelay).UnixMilli()
			bumped, bumpErr := q.BumpAvailableAt(ctx, in.Scope, in.UserID, delay, func(old []byte) []byte {
				var previous agent.JobPayload
				if json.Unmarshal(old, &previous) != nil || !previous.Addressed || addressed {
					return payload
				}
				// The merged job still answers a message aimed at the bot.
				merged, _ := json.Marshal(agent.JobPayload{MessageID: in.MessageID, SourceUpdateID: in.UpdateID, Edited: in.Edited, Addressed: true})
				return merged
			})
			if bumpErr != nil {
				return bumpErr
			}
			if bumped {
				return nil
			}
			// First message from this sender: enqueue with the coalescing window so
			// that a follow-up message can still bump available_at before the job fires.
			_, err = q.Enqueue(ctx, queue.Input{Scope: in.Scope, ActorUserID: in.UserID, SenderChatID: in.SenderChatID, Key: fmt.Sprintf("update:%d", in.UpdateID), Kind: "respond", Payload: payload, MaxAttempts: 3, AvailableAt: delay})
			return err
		}
		_, err = q.Enqueue(ctx, queue.Input{Scope: in.Scope, ActorUserID: in.UserID, SenderChatID: in.SenderChatID, Key: fmt.Sprintf("update:%d", in.UpdateID), Kind: "respond", Payload: payload, MaxAttempts: 3})
		return err
	})
	if err != nil {
		return errors.New("accepting Telegram update failed")
	}
	return nil
}

func (a *AgentStore) ChatBehavior(ctx context.Context, scope chat.Scope) (agent.ChatBehavior, error) {
	var behavior agent.ChatBehavior
	if !scope.Valid() {
		return behavior, errors.New("invalid conversation scope")
	}
	err := scoped(a.db.WithContext(ctx).Table("chat_directory"), scope).Select("agent_id,bot_id,chat_id,group_response,respond_to_bots,loop_protection,chat_tool_rounds,chat_tool_calls,allow_skip,auto_approve_corrections,behavior_version AS version").Take(&behavior).Error
	if err != nil {
		return behavior, errors.New("conversation behavior unavailable")
	}
	return behavior, nil
}

func (a *AgentStore) SetChatBehavior(ctx context.Context, behavior agent.ChatBehavior) error {
	scope := chat.Scope{AgentID: behavior.AgentID, BotID: behavior.BotID, ChatID: behavior.ChatID}
	if !scope.Valid() || behavior.Version <= 0 || (behavior.GroupResponse != "mentions" && behavior.GroupResponse != "all") {
		return errors.New("invalid conversation behavior")
	}
	if behavior.ChatToolRounds < 0 || behavior.ChatToolRounds > 32 || behavior.ChatToolCalls < 0 || behavior.ChatToolCalls > 64 {
		return errors.New("chat tool limits out of range")
	}
	result := scoped(a.db.WithContext(ctx).Table("chat_directory"), scope).Where("behavior_version=?", behavior.Version).Updates(map[string]any{
		"group_response":           behavior.GroupResponse,
		"respond_to_bots":          behavior.RespondToBots,
		"loop_protection":          behavior.LoopProtection,
		"chat_tool_rounds":         behavior.ChatToolRounds,
		"chat_tool_calls":          behavior.ChatToolCalls,
		"allow_skip":               behavior.AllowSkip,
		"auto_approve_corrections": behavior.AutoApproveCorrections,
		"behavior_version":         gorm.Expr("behavior_version+1"),
	})
	if result.Error != nil || result.RowsAffected != 1 {
		return errors.New("conversation behavior version conflict")
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
	if result.Summary.ThroughID > 0 {
		if err := memoryScoped(db.Table("conversation_messages"), s).Select("sent_at").Where("id=?", result.Summary.ThroughID).Scan(&result.Summary.ThroughAt).Error; err != nil {
			return result, errors.New("reading summary checkpoint failed")
		}
	}
	if err := memoryScoped(db.Table("conversation_messages"), s).Where("id>?", result.Summary.ThroughID).Count(&result.Summary.PendingCount).Error; err != nil {
		return result, errors.New("counting recent messages failed")
	}
	var knowledge struct{ Content string }
	err = db.Table("shared_knowledge").Where("agent_id=?", s.AgentID).Take(&knowledge).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return result, errors.New("reading knowledge failed")
	}
	result.Knowledge = knowledge.Content
	if err := memoriesWithUsers(db, s).Order("e.created_at DESC,e.source_message_id DESC").Limit(100).Find(&result.ExplicitMemories).Error; err != nil {
		return result, errors.New("reading explicit memory failed")
	}
	result.Reactions, err = loadReactions(db, s, 200)
	if err != nil {
		return result, err
	}
	query := historyWithUsers(db, s).Where("m.id>?", result.Summary.ThroughID)
	if through > 0 {
		query = query.Where("m.id<=?", through)
	}
	order := "m.id DESC"
	if through > 0 {
		order = "m.id ASC"
	}
	if err := query.Order(order).Limit(limit).Find(&result.Messages).Error; err != nil {
		return result, errors.New("reading history failed")
	}
	if through == 0 {
		for l, r := 0, len(result.Messages)-1; l < r; l, r = l+1, r-1 {
			result.Messages[l], result.Messages[r] = result.Messages[r], result.Messages[l]
		}
	}
	if err := attachQuotes(db, s, result.Messages); err != nil {
		return result, err
	}
	return result, nil
}

func hasImage(attachments []agent.Attachment) bool {
	for _, a := range attachments {
		if a.Kind == "image" {
			return true
		}
	}
	return false
}

// attachQuotes fills Quoted for replies whose target is outside the loaded
// window, so "this" in a reply is never a mystery to the model. Targets inside
// the window are visible by message_id already. Bounded: a few lookups at most.
func attachQuotes(db *gorm.DB, s chat.Scope, messages []agent.Message) error {
	inWindow := map[int64]bool{}
	for _, m := range messages {
		inWindow[m.MessageID] = true
	}
	missing := map[int64]bool{}
	for _, m := range messages {
		if m.ReplyToMessageID > 0 && !inWindow[m.ReplyToMessageID] {
			missing[m.ReplyToMessageID] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(missing))
	for id := range missing {
		ids = append(ids, id)
		if len(ids) == 8 {
			break
		}
	}
	var quoted []agent.Message
	if err := historyWithUsers(db, s).Where("m.message_id IN ?", ids).Find(&quoted).Error; err != nil {
		return errors.New("reading quoted messages failed")
	}
	byID := map[int64]agent.Message{}
	for _, q := range quoted {
		byID[q.MessageID] = q
	}
	for i := range messages {
		q, ok := byID[messages[i].ReplyToMessageID]
		if !ok || messages[i].ReplyToMessageID == 0 || inWindow[messages[i].ReplyToMessageID] {
			continue
		}
		text := q.Text
		if runes := []rune(text); len(runes) > 500 {
			text = string(runes[:500]) + "…"
		}
		messages[i].Quoted = fmt.Sprintf("[%s from=%s] %s", q.Role, q.Who(), text)
	}
	return nil
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

func (a *AgentStore) AdminDeleteExplicitMemory(ctx context.Context, scope chat.Scope, sourceMessageID int64) error {
	if !scope.Valid() || sourceMessageID <= 0 {
		return errors.New("invalid explicit memory")
	}
	result := memoryScoped(a.db.WithContext(ctx).Table("explicit_memories"), scope).Where("source_message_id=?", sourceMessageID).Delete(&struct{}{})
	if result.Error != nil || result.RowsAffected != 1 {
		return errors.New("explicit memory not found")
	}
	return nil
}

// UserProfileMemories projects the same explicit memories used by the agent
// into a person-centric view. There is intentionally no second profile store
// that could drift away from conversation memory.
func (a *AgentStore) UserProfileMemories(ctx context.Context, agentID string, userID int64) ([]agent.ProfileMemory, error) {
	if strings.TrimSpace(agentID) == "" || userID <= 0 {
		return nil, errors.New("invalid user profile")
	}
	rows := make([]agent.ProfileMemory, 0)
	err := a.db.WithContext(ctx).Raw(`SELECT e.agent_id,e.bot_id,e.chat_id,e.thread_id,c.title AS chat_title,
 e.source_message_id,e.content,e.created_at
 FROM explicit_memories e
 LEFT JOIN chat_directory c ON c.agent_id=e.agent_id AND c.bot_id=e.bot_id AND c.chat_id=e.chat_id
 WHERE e.agent_id=? AND e.created_by=?
 ORDER BY e.created_at DESC,e.source_message_id DESC LIMIT 200`, agentID, userID).Scan(&rows).Error
	if err != nil {
		return nil, errors.New("loading user profile failed")
	}
	return rows, nil
}

func (a *AgentStore) TelegramProfile(ctx context.Context, agentID string, userID int64) (agent.TelegramProfile, error) {
	var profile agent.TelegramProfile
	if strings.TrimSpace(agentID) == "" || userID <= 0 {
		return profile, errors.New("invalid user profile")
	}
	err := a.db.WithContext(ctx).Table("seen_users").Where("agent_id=? AND user_id=?", agentID, userID).Take(&profile).Error
	if err != nil {
		return profile, errors.New("loading Telegram profile failed")
	}
	return profile, nil
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
	if err := memoriesWithUsers(a.db.WithContext(ctx), scope).Where("e.embedding_model=?", model).Order("e.created_at DESC,e.source_message_id DESC").Limit(500).Find(&rows).Error; err != nil {
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

// SimilarExplicitMemory returns the current user's closest remembered fact in
// this conversation. The caller chooses the threshold; a zero score means no
// comparable embedding was found.
func (a *AgentStore) SimilarExplicitMemory(ctx context.Context, scope chat.Scope, userID int64, model string, query []float32) (agent.ExplicitMemory, float64, error) {
	if userID <= 0 {
		return agent.ExplicitMemory{}, 0, errors.New("invalid explicit memory owner")
	}
	encoded, err := encodeEmbedding(model, query)
	if err != nil {
		return agent.ExplicitMemory{}, 0, err
	}
	var normalized []float32
	if json.Unmarshal([]byte(encoded), &normalized) != nil {
		return agent.ExplicitMemory{}, 0, errors.New("invalid explicit memory search")
	}
	var rows []agent.ExplicitMemory
	if err := memoriesWithUsers(a.db.WithContext(ctx), scope).Where("e.created_by=? AND e.embedding_model=?", userID, model).Order("e.created_at DESC").Limit(500).Find(&rows).Error; err != nil {
		return agent.ExplicitMemory{}, 0, errors.New("searching explicit memory failed")
	}
	var best agent.ExplicitMemory
	bestScore := 0.0
	for _, row := range rows {
		var vector []float32
		if json.Unmarshal([]byte(row.Embedding), &vector) != nil || len(vector) != len(normalized) {
			continue
		}
		score := 0.0
		for i := range vector {
			score += float64(vector[i] * normalized[i])
		}
		if best.SourceMessageID == 0 || score > bestScore {
			best, bestScore = row, score
		}
	}
	return best, bestScore, nil
}

// SaveSummary persists a new compaction summary and returns the new
// compaction_count for this chat (monotonically incremented on each save).
func (a *AgentStore) SaveSummary(ctx context.Context, job queue.Job, previous, through int64, content string, revision ...int64) (int64, error) {
	if len(content) == 0 || len(content) > 16000 || through <= previous {
		return 0, errors.New("invalid summary")
	}
	var count int64
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
		var row struct{ CompactionCount int64 }
		if err := scoped(tx.Table("chat_directory"), job.Scope).Select("compaction_count").Take(&row).Error; err != nil {
			return errors.New("reading compaction count failed")
		}
		count = row.CompactionCount + 1
		if err := scoped(tx.Table("chat_directory"), job.Scope).Update("compaction_count", count).Error; err != nil {
			return errors.New("incrementing compaction count failed")
		}
		return nil
	})
	return count, err
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
	if err := historyWithUsers(a.db.WithContext(ctx), s).Where("m.id>? AND m.id<=?", c.Summary.ThroughID, target.ID).Order("m.id DESC").Limit(limit).Find(&c.Messages).Error; err != nil {
		return c, time.Time{}, errors.New("reply context unavailable")
	}
	for l, r := 0, len(c.Messages)-1; l < r; l, r = l+1, r-1 {
		c.Messages[l], c.Messages[r] = c.Messages[r], c.Messages[l]
	}
	if err := attachQuotes(a.db.WithContext(ctx), s, c.Messages); err != nil {
		return c, time.Time{}, err
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

// BotMessagePreviews returns bounded text for known outgoing-message IDs in one
// conversation. It lets the agent select an edit/delete target by content
// instead of guessing between several messages sent close together.
func (a *AgentStore) BotMessagePreviews(ctx context.Context, s chat.Scope, ids []int64) (map[int64]string, error) {
	previews := make(map[int64]string, len(ids))
	if !s.Valid() || len(ids) > 50 {
		return nil, errors.New("invalid bot message preview query")
	}
	if len(ids) == 0 {
		return previews, nil
	}
	var rows []struct {
		MessageID int64
		Text      string
	}
	if err := memoryScoped(a.db.WithContext(ctx).Table("conversation_messages"), s).Select("message_id,text").Where("role='assistant' AND message_id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, errors.New("reading bot message previews failed")
	}
	for _, row := range rows {
		value := []rune(strings.TrimSpace(row.Text))
		if len(value) > 160 {
			value = append(value[:159], '…')
		}
		previews[row.MessageID] = string(value)
	}
	return previews, nil
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

// RecordToolMessage adds a bot message sent by an agent tool to conversation
// history. The BotMessages ledger is recorded separately around the external
// Telegram call; this write is fenced by the response job lease.
func (a *AgentStore) RecordToolMessage(ctx context.Context, job queue.Job, messageID int64, text string, at time.Time) error {
	if messageID <= 0 || strings.TrimSpace(text) == "" || at.IsZero() {
		return errors.New("invalid tool message")
	}
	return a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		result := tx.Exec(`INSERT INTO conversation_messages (agent_id,bot_id,chat_id,thread_id,message_id,user_id,role,text,sent_at)
 VALUES (?,?,?,?,?,0,'assistant',?,?) ON CONFLICT(agent_id,bot_id,chat_id,message_id) DO NOTHING`, job.Scope.AgentID, job.Scope.BotID, job.Scope.ChatID, job.Scope.ThreadID, messageID, strings.TrimSpace(text), at.UnixMilli())
		if result.Error != nil || result.RowsAffected != 1 {
			return errors.New("recording tool message failed")
		}
		return nil
	})
}

// DeleteExplicitMemory removes one fact of this chat by its source message.
// Only the author can delete their own fact through tools.
func (a *AgentStore) DeleteExplicitMemory(ctx context.Context, job queue.Job, sourceMessageID int64) (bool, error) {
	if sourceMessageID <= 0 || job.ActorUserID <= 0 {
		return false, errors.New("invalid explicit memory source")
	}
	deleted := false
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		store := &AgentStore{db: tx}
		if err := store.CheckActions(ctx, job.Scope, job.ActorUserID); err != nil {
			return err
		}
		result := memoryScoped(tx.Table("explicit_memories"), job.Scope).Where("source_message_id=? AND created_by=?", sourceMessageID, job.ActorUserID).Delete(&struct{}{})
		if result.Error != nil {
			return errors.New("deleting explicit memory failed")
		}
		deleted = result.RowsAffected > 0
		return nil
	})
	return deleted, err
}

// UpdateExplicitMemory replaces a fact without changing its stable source ID.
// Only its author may update it through an agent tool.
func (a *AgentStore) UpdateExplicitMemory(ctx context.Context, job queue.Job, sourceMessageID int64, content, embeddingModel string, embedding []float32) (bool, error) {
	if sourceMessageID <= 0 || job.ActorUserID <= 0 || strings.TrimSpace(content) == "" || len(content) > 2000 {
		return false, errors.New("invalid explicit memory")
	}
	encoded, err := encodeEmbedding(embeddingModel, embedding)
	if err != nil {
		return false, err
	}
	updated := false
	err = a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := fence(tx, job); err != nil {
			return err
		}
		store := &AgentStore{db: tx}
		if err := store.CheckActions(ctx, job.Scope, job.ActorUserID); err != nil {
			return err
		}
		result := memoryScoped(tx.Table("explicit_memories"), job.Scope).Where("source_message_id=? AND created_by=?", sourceMessageID, job.ActorUserID).Updates(map[string]any{"content": strings.TrimSpace(content), "embedding_model": embeddingModel, "embedding": encoded, "created_at": time.Now().UTC().UnixMilli()})
		if result.Error != nil {
			return errors.New("updating explicit memory failed")
		}
		updated = result.RowsAffected > 0
		return nil
	})
	return updated, err
}

type choiceSessionRow struct {
	ID            int64  `gorm:"column:id"`
	AgentID       string `gorm:"column:agent_id"`
	BotID         int64  `gorm:"column:bot_id"`
	ChatID        int64  `gorm:"column:chat_id"`
	ThreadID      int64  `gorm:"column:thread_id"`
	UserID        int64  `gorm:"column:user_id"`
	MessageID     int64  `gorm:"column:message_id"`
	Question      string `gorm:"column:question"`
	Options       string `gorm:"column:options"`
	Multi         int    `gorm:"column:multi"`
	Selected      string `gorm:"column:selected"`
	State         string `gorm:"column:state"`
	ContextCursor int64  `gorm:"column:context_cursor"`
	ExpiresAt     int64  `gorm:"column:expires_at"`
}

func rowToChoiceSession(r choiceSessionRow) *agent.ChoiceSession {
	sess := &agent.ChoiceSession{
		ID:            r.ID,
		AgentID:       r.AgentID,
		BotID:         r.BotID,
		ChatID:        r.ChatID,
		ThreadID:      r.ThreadID,
		UserID:        r.UserID,
		MessageID:     r.MessageID,
		Question:      r.Question,
		Multi:         r.Multi != 0,
		State:         r.State,
		ContextCursor: r.ContextCursor,
		ExpiresAt:     r.ExpiresAt,
	}
	_ = json.Unmarshal([]byte(r.Options), &sess.Options)
	_ = json.Unmarshal([]byte(r.Selected), &sess.Selected)
	return sess
}

func (a *AgentStore) MaxConversationID(ctx context.Context, s chat.Scope) (int64, error) {
	var id int64
	err := memoryScoped(a.db.WithContext(ctx).Table("conversation_messages"), s).Select("COALESCE(MAX(id),0)").Scan(&id).Error
	return id, err
}

// HasNewerUserMessage reports whether the scope has any user message with a
// Telegram message_id greater than afterMessageID. Used to decide whether to
// quote the triggering message so the reply stays anchored in the thread.
func (a *AgentStore) HasNewerUserMessage(ctx context.Context, s chat.Scope, afterMessageID int64) (bool, error) {
	var count int64
	err := scoped(a.db.WithContext(ctx).Table("conversation_messages"), s).
		Where("thread_id=? AND message_id>? AND role='user'", s.ThreadID, afterMessageID).
		Count(&count).Error
	return count > 0, err
}

func (a *AgentStore) CreateChoiceSession(ctx context.Context, sess agent.ChoiceSession) (int64, error) {
	if sess.AgentID == "" || sess.BotID == 0 || sess.ChatID == 0 || sess.UserID == 0 || len(sess.Options) == 0 {
		return 0, errors.New("invalid choice session")
	}
	optJSON, _ := json.Marshal(sess.Options)
	now := time.Now().Unix()
	expires := sess.ExpiresAt
	if expires == 0 {
		expires = now + 86400
	}
	row := map[string]any{
		"agent_id":       sess.AgentID,
		"bot_id":         sess.BotID,
		"chat_id":        sess.ChatID,
		"thread_id":      sess.ThreadID,
		"user_id":        sess.UserID,
		"message_id":     0,
		"question":       sess.Question,
		"options":        string(optJSON),
		"multi":          boolToInt(sess.Multi),
		"selected":       "[]",
		"state":          "open",
		"context_cursor": sess.ContextCursor,
		"created_at":     now,
		"expires_at":     expires,
	}
	res := a.db.WithContext(ctx).Table("choice_sessions").Create(row)
	if res.Error != nil {
		return 0, errors.New("creating choice session failed")
	}
	var id int64
	a.db.WithContext(ctx).Table("choice_sessions").
		Where("agent_id=? AND bot_id=? AND chat_id=? AND user_id=? AND created_at=?", sess.AgentID, sess.BotID, sess.ChatID, sess.UserID, now).
		Select("id").Order("id DESC").Limit(1).Scan(&id)
	return id, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (a *AgentStore) SetChoiceMessageID(ctx context.Context, id, messageID int64) error {
	return a.db.WithContext(ctx).Table("choice_sessions").Where("id=?", id).Update("message_id", messageID).Error
}

func (a *AgentStore) GetChoiceSession(ctx context.Context, id int64) (*agent.ChoiceSession, error) {
	var row choiceSessionRow
	err := a.db.WithContext(ctx).Table("choice_sessions").Where("id=?", id).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("loading choice session failed")
	}
	return rowToChoiceSession(row), nil
}

func (a *AgentStore) DeleteChoiceSession(ctx context.Context, id int64) error {
	return a.db.WithContext(ctx).Table("choice_sessions").Delete(nil, "id=?", id).Error
}

func (a *AgentStore) ExpiredChoiceSessions(ctx context.Context, limit int) ([]agent.ChoiceSession, error) {
	var rows []choiceSessionRow
	now := time.Now().Unix()
	err := a.db.WithContext(ctx).Table("choice_sessions").
		Where("state='open' AND expires_at<=? AND message_id>0", now).
		Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, errors.New("loading expired sessions failed")
	}
	out := make([]agent.ChoiceSession, len(rows))
	for i, r := range rows {
		out[i] = *rowToChoiceSession(r)
	}
	return out, nil
}

func (a *AgentStore) AcceptCallback(ctx context.Context, agentID string, botID int64, cb agent.Callback) (agent.CallbackResult, error) {
	ignore := agent.CallbackResult{Action: "ignore"}
	if !strings.HasPrefix(cb.Data, "c:") {
		return ignore, nil
	}
	rest := cb.Data[2:]
	colon := strings.LastIndex(rest, ":")
	if colon < 0 {
		return ignore, nil
	}
	sessionIDStr := rest[:colon]
	action := rest[colon+1:]
	sessionID, err := parseInt64(sessionIDStr)
	if err != nil || sessionID <= 0 {
		return ignore, nil
	}

	var result agent.CallbackResult
	txErr := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row choiceSessionRow
		if err := tx.Table("choice_sessions").Where("id=?", sessionID).Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				result = ignore
				return nil
			}
			return err
		}
		if row.AgentID != agentID || row.BotID != botID {
			result = ignore
			return nil
		}
		if row.State != "open" {
			result = ignore
			return nil
		}
		if time.Now().Unix() > row.ExpiresAt {
			tx.Table("choice_sessions").Delete(nil, "id=?", sessionID)
			result = agent.CallbackResult{Action: "expired", Session: rowToChoiceSession(row)}
			return nil
		}
		if row.UserID != cb.UserID {
			result = agent.CallbackResult{Action: "toast", Toast: "This question is not for you."}
			return nil
		}

		var options []string
		json.Unmarshal([]byte(row.Options), &options)
		var selected []int
		json.Unmarshal([]byte(row.Selected), &selected)
		multi := row.Multi != 0

		if action == "done" {
			if !multi {
				result = ignore
				return nil
			}
			if len(selected) == 0 {
				result = agent.CallbackResult{Action: "toast", Toast: "Select at least one option."}
				return nil
			}
			if err := tx.Table("choice_sessions").Where("id=?", sessionID).Update("state", "done").Error; err != nil {
				return err
			}
			scope := chat.Scope{AgentID: row.AgentID, BotID: row.BotID, ChatID: row.ChatID, ThreadID: row.ThreadID}
			jobPayload, _ := json.Marshal(agent.JobPayload{ChoiceSessionID: sessionID, MessageID: row.MessageID})
			_, enqErr := (&Database{orm: tx}).Queue().Enqueue(ctx, queue.Input{Scope: scope, ActorUserID: row.UserID, Key: fmt.Sprintf("choice:%d", sessionID), Kind: "respond", Payload: jobPayload, MaxAttempts: 3})
			if enqErr != nil {
				return enqErr
			}
			row.State = "done"
			result = agent.CallbackResult{Action: "done", Session: rowToChoiceSession(row)}
			return nil
		}

		idx, parseErr := parseInt(action)
		if parseErr != nil || idx < 0 || idx >= len(options) {
			result = ignore
			return nil
		}

		if !multi {
			sel, _ := json.Marshal([]int{idx})
			if err := tx.Table("choice_sessions").Where("id=?", sessionID).Updates(map[string]any{"state": "done", "selected": string(sel)}).Error; err != nil {
				return err
			}
			scope := chat.Scope{AgentID: row.AgentID, BotID: row.BotID, ChatID: row.ChatID, ThreadID: row.ThreadID}
			jobPayload, _ := json.Marshal(agent.JobPayload{ChoiceSessionID: sessionID, MessageID: row.MessageID})
			_, enqErr := (&Database{orm: tx}).Queue().Enqueue(ctx, queue.Input{Scope: scope, ActorUserID: row.UserID, Key: fmt.Sprintf("choice:%d", sessionID), Kind: "respond", Payload: jobPayload, MaxAttempts: 3})
			if enqErr != nil {
				return enqErr
			}
			row.Selected = string(sel)
			row.State = "done"
			result = agent.CallbackResult{Action: "done", Session: rowToChoiceSession(row)}
			return nil
		}

		// Multi-select: toggle
		newSel := toggleIndex(selected, idx)
		selJSON, _ := json.Marshal(newSel)
		if err := tx.Table("choice_sessions").Where("id=?", sessionID).Update("selected", string(selJSON)).Error; err != nil {
			return err
		}
		row.Selected = string(selJSON)
		result = agent.CallbackResult{Action: "toggle", Session: rowToChoiceSession(row)}
		return nil
	})
	return result, txErr
}

func toggleIndex(selected []int, idx int) []int {
	for i, v := range selected {
		if v == idx {
			return append(selected[:i], selected[i+1:]...)
		}
	}
	return append(selected, idx)
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		n = n*10 + int64(c-'0')
	}
	if s == "" {
		return 0, errors.New("empty")
	}
	return n, nil
}

func parseInt(s string) (int, error) {
	v, err := parseInt64(s)
	return int(v), err
}
