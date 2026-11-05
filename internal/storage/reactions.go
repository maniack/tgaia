package storage

import (
	"context"
	"errors"
	"strings"

	"github.com/maniack/tgaia/internal/access"
	"github.com/maniack/tgaia/internal/agent"
	"github.com/maniack/tgaia/internal/chat"
	"gorm.io/gorm"
)

func validReactionUpdate(update agent.ReactionUpdate) bool {
	if !validAgent(update.AgentID) || update.BotID <= 0 || update.ChatID == 0 || update.MessageID <= 0 || update.UpdateID < 0 || update.UpdatedAt.IsZero() || len(update.Reactions) > 20 {
		return false
	}
	if (update.ActorType == "anonymous" && update.ActorID != 0) || ((update.ActorType == "user" || update.ActorType == "sender_chat") && update.ActorID == 0) {
		return false
	}
	if update.ActorType != "user" && update.ActorType != "sender_chat" && update.ActorType != "anonymous" {
		return false
	}
	if len(update.ActorName) > 512 || len(update.ActorUsername) > 64 {
		return false
	}
	for _, reaction := range update.Reactions {
		if strings.TrimSpace(reaction.Type) == "" || len(reaction.Type) > 64 || len(reaction.Value) > 256 || reaction.Count < 1 || reaction.Count > 1_000_000_000 {
			return false
		}
	}
	return true
}

// ApplyReaction replaces one actor's reaction set on a known bot-authored
// message. It deliberately does not enqueue work: reactions become context
// only when a later message causes normal agent execution.
func (a *AgentStore) ApplyReaction(ctx context.Context, update agent.ReactionUpdate) error {
	if !validReactionUpdate(update) {
		return errors.New("invalid reaction update")
	}
	scope := chat.Scope{AgentID: update.AgentID, BotID: update.BotID, ChatID: update.ChatID}
	err := a.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Table("bot_messages").Where("agent_id=? AND bot_id=? AND chat_id=? AND message_id=? AND deleted_at IS NULL", update.AgentID, update.BotID, update.ChatID, update.MessageID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			return nil
		}
		actorUserID := int64(0)
		if update.ActorType == "user" {
			actorUserID = update.ActorID
		}
		if err := (&AgentStore{db: tx}).Check(ctx, scope, actorUserID); err != nil {
			if errors.Is(err, access.ErrBlocked) || errors.Is(err, ErrChatBlocked) {
				return nil
			}
			return err
		}
		if update.ActorType == "user" {
			if err := tx.Exec(`INSERT INTO seen_users (agent_id,user_id,username,name) VALUES (?,?,?,?)
 ON CONFLICT (agent_id,user_id) DO UPDATE SET username=excluded.username,name=excluded.name`, update.AgentID, update.ActorID, update.ActorUsername, update.ActorName).Error; err != nil {
				return err
			}
		}
		applied := tx.Exec(`INSERT INTO bot_reaction_states (agent_id,bot_id,chat_id,message_id,actor_type,actor_id,update_id,actor_name,actor_username,updated_at)
 VALUES (?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT (agent_id,bot_id,chat_id,message_id,actor_type,actor_id) DO UPDATE SET
 update_id=excluded.update_id,actor_name=excluded.actor_name,actor_username=excluded.actor_username,updated_at=excluded.updated_at
 WHERE bot_reaction_states.update_id < excluded.update_id`, update.AgentID, update.BotID, update.ChatID, update.MessageID, update.ActorType, update.ActorID, update.UpdateID, update.ActorName, update.ActorUsername, update.UpdatedAt.UnixMilli())
		if applied.Error != nil {
			return applied.Error
		}
		if applied.RowsAffected == 0 {
			return nil
		}
		if err := tx.Exec("DELETE FROM bot_message_reactions WHERE agent_id=? AND bot_id=? AND chat_id=? AND message_id=? AND actor_type=? AND actor_id=?", update.AgentID, update.BotID, update.ChatID, update.MessageID, update.ActorType, update.ActorID).Error; err != nil {
			return err
		}
		for _, reaction := range update.Reactions {
			if err := tx.Exec(`INSERT INTO bot_message_reactions (agent_id,bot_id,chat_id,message_id,actor_type,actor_id,reaction_type,reaction_value,total_count)
 VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT (agent_id,bot_id,chat_id,message_id,actor_type,actor_id,reaction_type,reaction_value)
 DO UPDATE SET total_count=excluded.total_count`, update.AgentID, update.BotID, update.ChatID, update.MessageID, update.ActorType, update.ActorID, reaction.Type, reaction.Value, reaction.Count).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return errors.New("storing message reaction failed")
	}
	return nil
}

func loadReactions(db *gorm.DB, scope chat.Scope, limit int) ([]agent.MessageReaction, error) {
	var reactions []agent.MessageReaction
	query := `SELECT r.message_id,r.actor_type,r.actor_id,
 COALESCE(NULLIF(u.name,''),s.actor_name) AS actor_name,
 COALESCE(NULLIF(u.username,''),s.actor_username) AS actor_username,
 r.reaction_type AS type,r.reaction_value AS value,r.total_count AS count,s.updated_at,
 COALESCE(m.text,'') AS message_text
 FROM bot_message_reactions r
 JOIN bot_reaction_states s ON s.agent_id=r.agent_id AND s.bot_id=r.bot_id AND s.chat_id=r.chat_id AND s.message_id=r.message_id AND s.actor_type=r.actor_type AND s.actor_id=r.actor_id
 JOIN bot_messages b ON b.agent_id=r.agent_id AND b.bot_id=r.bot_id AND b.chat_id=r.chat_id AND b.message_id=r.message_id AND b.deleted_at IS NULL
 LEFT JOIN conversation_messages m ON m.agent_id=r.agent_id AND m.bot_id=r.bot_id AND m.chat_id=r.chat_id AND m.message_id=r.message_id AND m.role='assistant'
 LEFT JOIN seen_users u ON u.agent_id=s.agent_id AND s.actor_type='user' AND u.user_id=s.actor_id
 LEFT JOIN blocked_users x ON x.agent_id=s.agent_id AND s.actor_type='user' AND x.user_id=s.actor_id
 WHERE r.agent_id=? AND r.bot_id=? AND r.chat_id=? AND x.user_id IS NULL
 ORDER BY s.updated_at DESC,r.message_id DESC,r.actor_type,r.actor_id,r.reaction_type,r.reaction_value LIMIT ?`
	if err := db.Raw(query, scope.AgentID, scope.BotID, scope.ChatID, limit).Scan(&reactions).Error; err != nil {
		return nil, errors.New("reading message reactions failed")
	}
	for i := range reactions {
		runes := []rune(reactions[i].MessageText)
		if len(runes) > 500 {
			reactions[i].MessageText = string(runes[:500])
		}
	}
	return reactions, nil
}
